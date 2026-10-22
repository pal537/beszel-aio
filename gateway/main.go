// beszel-gateway: Beszel 的 fnOS 包装层管家程序
// 职责: 拉起并守护 Beszel hub 与 agent、首次启动自动完成本机配对、
// 在应用入口提供自动登录(免注册免登录)、保留设置页。
// Beszel 本体为上游官方二进制, 本程序不修改其任何行为, 仅通过其
// 官方 CLI / API / config.yml 同步机制完成编排。
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const hubPort = 18093

var (
	dataDir  string
	appDir   string
	port     string
	sockPath string
	prefix   string
	state    *State
	stateMu  sync.Mutex
	ready    bool
	readyMu  sync.RWMutex
	hubCmd   *exec.Cmd
	agentCmd *exec.Cmd
	procMu   sync.Mutex
)

type State struct {
	SUEmail    string `json:"su_email"`
	SUPass     string `json:"su_pass"`
	UserEmail  string `json:"user_email"`
	UserPass   string `json:"user_pass"`
	AgentToken string `json:"agent_token"`
	HubPubKey  string `json:"hub_pubkey"`
	Paired     bool   `json:"paired"`
	AutoLogin  bool   `json:"autologin"`
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func statePath() string { return filepath.Join(dataDir, "gateway-state.json") }

func loadState() *State {
	s := &State{AutoLogin: true}
	raw, err := os.ReadFile(statePath())
	if err == nil {
		_ = json.Unmarshal(raw, s)
	}
	if s.SUEmail == "" {
		s.SUEmail = "su@beszel.local"
		s.SUPass = "Su-" + randHex(16)
		s.UserEmail = "admin@beszel.local"
		s.UserPass = randHex(16)
		s.AgentToken = "aio-" + randHex(20)
		saveState(s)
	}
	return s
}

func saveState(s *State) {
	raw, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(statePath(), raw, 0600)
}

func binPath(name string) string { return filepath.Join(appDir, "bin", name) }
func hubDataDir() string        { return filepath.Join(dataDir, "beszel_data") }
func logDir() string            { return filepath.Join(dataDir, "logs") }

func startProc(name string, args []string, cwd, logName string, env ...string) (*exec.Cmd, error) {
	f, err := os.OpenFile(filepath.Join(logDir(), logName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binPath(name), args...)
	cmd.Dir = cwd
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func stopProc(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func hubHealth() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", hubPort))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func waitHub() bool {
	for i := 0; i < 60; i++ {
		if hubHealth() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func startHub() error {
	procMu.Lock()
	defer procMu.Unlock()
	cmd, err := startProc("beszel", []string{"serve", "--http", fmt.Sprintf("127.0.0.1:%d", hubPort), "--dir", hubDataDir()}, dataDir, "hub.log")
	if err != nil {
		return err
	}
	hubCmd = cmd
	go func() {
		_ = cmd.Wait()
		log.Printf("hub exited")
	}()
	if !waitHub() {
		return fmt.Errorf("hub did not become healthy")
	}
	return nil
}

func restartHub() error {
	procMu.Lock()
	old := hubCmd
	procMu.Unlock()
	stopProc(old)
	return startHub()
}

// api performs an HTTP call against the hub API and returns status + body.
func api(method, path, token string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", hubPort, path), body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

func superuserToken() (string, error) {
	code, raw, err := api("POST", "/api/collections/_superusers/auth-with-password", "", map[string]string{
		"identity": state.SUEmail, "password": state.SUPass,
	})
	if err != nil || code != 200 {
		return "", fmt.Errorf("superuser auth failed: code=%d err=%v", code, err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("superuser auth: bad response")
	}
	return out.Token, nil
}

// ensureManagedUser creates the built-in local account (idempotent).
func ensureManagedUser(su string) error {
	code, raw, err := api("GET", "/api/collections/users/records?perPage=50", su, nil)
	if err != nil || code != 200 {
		return fmt.Errorf("list users: code=%d err=%v", code, err)
	}
	var list struct {
		Items []struct {
			Email string `json:"email"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &list)
	for _, it := range list.Items {
		if it.Email == state.UserEmail {
			return nil
		}
	}
	code, raw, err = api("POST", "/api/collections/users/records", su, map[string]any{
		"email": state.UserEmail, "password": state.UserPass, "passwordConfirm": state.UserPass,
		"verified": true, "role": "admin", "name": "Admin",
	})
	if err != nil || (code != 200 && code != 201) {
		// retry without role in case this build restricts it
		code, raw, err = api("POST", "/api/collections/users/records", su, map[string]any{
			"email": state.UserEmail, "password": state.UserPass, "passwordConfirm": state.UserPass,
			"verified": true, "name": "Admin",
		})
		if err != nil || (code != 200 && code != 201) {
			return fmt.Errorf("create user: code=%d body=%s", code, string(raw))
		}
	}
	return nil
}

// pair performs the one-time local pairing using Beszel's config.yml sync.
func pair(su string) error {
	cfg := fmt.Sprintf("systems:\n  - name: 本机监控\n    host: 127.0.0.1\n    token: %s\n", state.AgentToken)
	cfgPath := filepath.Join(hubDataDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		return err
	}
	if err := restartHub(); err != nil {
		return err
	}
	defer os.Remove(cfgPath)
	code, raw, err := api("GET", "/api/collections/systems/records", su, nil)
	if err != nil || code != 200 {
		return fmt.Errorf("list systems: code=%d err=%v", code, err)
	}
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &list)
	found := false
	for _, it := range list.Items {
		if it.Name == "本机监控" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("system not created by config sync")
	}
	code, raw, err = api("GET", "/api/beszel/info", su, nil)
	if err != nil || code != 200 {
		return fmt.Errorf("get hub info: code=%d err=%v", code, err)
	}
	var info struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.Key == "" {
		return fmt.Errorf("hub pubkey missing")
	}
	stateMu.Lock()
	state.HubPubKey = info.Key
	state.Paired = true
	saveState(state)
	stateMu.Unlock()
	return nil
}

func startAgent() {
	args := []string{"-u", fmt.Sprintf("http://127.0.0.1:%d", hubPort), "-t", state.AgentToken, "-k", state.HubPubKey}
	agentDir := filepath.Join(dataDir, "agent")
	_ = os.MkdirAll(agentDir, 0755)
	for {
		cmd, err := startProc("beszel-agent", args, agentDir, "agent.log")
		if err != nil {
			log.Printf("agent start failed: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		procMu.Lock()
		agentCmd = cmd
		procMu.Unlock()
		err = cmd.Wait()
		log.Printf("agent exited: %v; restarting in 5s", err)
		select {
		case <-shutdownCh:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// ---------- TLS: 复用飞牛证书库里最新的一张证书 ----------
// 飞牛桌面走 HTTPS 时, http 内嵌窗口会被浏览器按混合内容拦掉, 所以窗口模式必须说 HTTPS。
// 证书来源: /usr/trim/var/trim_connect/ssls/<域名>/<时间戳>/<域名>.crt|.key,
// 取整体最新的一份; 每次握手检查指纹, 变了就重载 —— 自动跟上续签。
// 找不到证书时回退明文 HTTP(局域网直连场景仍可用)。

var (
	certMu      sync.Mutex
	certCache   *tls.Certificate
	certCacheFP string
)

type certCandidate struct {
	dom     string
	crt     string
	key     string
	modTime time.Time
}

// scanCerts 列出证书库里每个域名最新的一份证书。
func scanCerts() []certCandidate {
	base := os.Getenv("BZ_CERT_DIR")
	if base == "" {
		base = "/usr/trim/var/trim_connect/ssls"
	}
	doms, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []certCandidate
	for _, d := range doms {
		if !d.IsDir() {
			continue
		}
		subs, err := os.ReadDir(filepath.Join(base, d.Name()))
		if err != nil {
			continue
		}
		var best *certCandidate
		for _, s := range subs {
			if !s.IsDir() {
				continue
			}
			info, err := s.Info()
			if err != nil {
				continue
			}
			crt := filepath.Join(base, d.Name(), s.Name(), d.Name()+".crt")
			key := filepath.Join(base, d.Name(), s.Name(), d.Name()+".key")
			if _, err := os.Stat(crt); err != nil {
				continue
			}
			if _, err := os.Stat(key); err != nil {
				continue
			}
			if best == nil || info.ModTime().After(best.modTime) {
				c := certCandidate{dom: d.Name(), crt: crt, key: key, modTime: info.ModTime()}
				best = &c
			}
		}
		if best != nil {
			out = append(out, *best)
		}
	}
	return out
}

func certFiles() (string, string, bool) {
	cands := scanCerts()
	if len(cands) == 0 {
		return "", "", false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.modTime.After(best.modTime) {
			best = c
		}
	}
	return best.crt, best.key, true
}

// getCertificate 按 SNI 域名对号入座: 访问哪个域名就发哪个域名的证书;
// 无 SNI 或无匹配时退回最新的一张。选错证书会被浏览器以域名不符拒绝,
// 这是窗口版打不开的根因(库里其他域名的证书更新时, 按"最新"挑必错)。
func getCertificate(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cands := scanCerts()
	if len(cands) == 0 {
		return nil, fmt.Errorf("no cert available")
	}
	want := ""
	if hi != nil && hi.Conn != nil {
		want = hi.ServerName
	}
	if forced := os.Getenv("BZ_CERT_DOMAIN"); forced != "" {
		want = forced
	}
	pick := cands[0]
	matched := false
	for _, c := range cands {
		if want != "" && c.dom == want {
			pick = c
			matched = true
			break
		}
		if c.modTime.After(pick.modTime) {
			pick = c
		}
	}
	_ = matched
	fp := pick.crt + "|" + pick.key
	if st, err := os.Stat(pick.crt); err == nil {
		fp += "|" + strconv.FormatInt(st.ModTime().Unix(), 10)
	}
	certMu.Lock()
	defer certMu.Unlock()
	if certCache != nil && certCacheFP == fp {
		return certCache, nil
	}
	c, err := tls.LoadX509KeyPair(pick.crt, pick.key)
	if err != nil {
		if certCache != nil {
			return certCache, nil
		}
		return nil, err
	}
	log.Printf("serving cert for domain %s", pick.dom)
	certCache = &c
	certCacheFP = fp
	return certCache, nil
}

// ---------- 同源网关模式(/app/beszel-aio 前缀)正文改写 ----------
// hub(PocketBase + Beszel 前端)按根路径写死了 /assets /static /api /_ 等
// 绝对路径, 挂到飞牛桌面 /app/<app> 前缀下时浏览器会把这些路径解析到桌面
// 根目录而 404。网关在转发时给这些字面量补上前缀; hub 本体不改。
// SSE(text/event-stream)与二进制内容不在改写之列。

func rewritableType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, t := range []string{"text/html", "text/css", "javascript", "application/json", "manifest+json", "image/svg+xml"} {
		if strings.Contains(ct, t) {
			return true
		}
	}
	return false
}

var rewriteRoots = []string{"/assets/", "/static/", "/api/", "/_/"}

func rewriteBody(b []byte, ct string) []byte {
	s := string(b)
	for _, root := range rewriteRoots {
		for _, lead := range []string{"\"", "'", "`", "("} {
			s = strings.ReplaceAll(s, lead+root, lead+prefix+root)
		}
	}
	if strings.Contains(ct, "text/html") {
		// SPA 路由用的 pushState/replaceState 与站内链接若写根路径会跳出前缀,
		// 注入一小段守卫脚本把根路径自动补上前缀
		guard := `<script>(function(){var P="` + prefix + `";function fix(u){if(typeof u==="string"&&u.charAt(0)==="/"&&u.charAt(1)!=="/"&&u!==P&&u.indexOf(P+"/")!==0){return P+u}return u}var ps=history.pushState,rs=history.replaceState;history.pushState=function(a,b,u){return ps.call(this,a,b,fix(u))};history.replaceState=function(a,b,u){return rs.call(this,a,b,fix(u))};document.addEventListener("click",function(e){var t=e.target;if(t&&t.closest){var a=t.closest("a");if(a){var h=a.getAttribute("href");if(h&&h.charAt(0)==="/"&&h.charAt(1)!=="/"){a.setAttribute("href",fix(h))}}}},true);})();</script>`
		low := strings.ToLower(s)
		if i := strings.Index(low, "<head"); i >= 0 {
			if j := strings.Index(low[i:], ">"); j >= 0 {
				s = s[:i+j+1] + guard + s[i+j+1:]
			}
		}
	}
	return []byte(s)
}

var shutdownCh = make(chan struct{})

func bootstrap() {
	_ = os.MkdirAll(hubDataDir(), 0755)
	_ = os.MkdirAll(logDir(), 0755)

	// 首次启动: 在 hub 启动前用官方 CLI 写入内部超级用户
	if _, err := os.Stat(filepath.Join(hubDataDir(), "data.db")); os.IsNotExist(err) {
		cmd := exec.Command(binPath("beszel"), "superuser", "upsert", state.SUEmail, state.SUPass, "--dir", hubDataDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("superuser upsert failed: %v (%s)", err, string(out))
		}
	}
	if err := startHub(); err != nil {
		log.Printf("hub start failed: %v", err)
		return
	}
	su, err := superuserToken()
	if err != nil {
		log.Printf("bootstrap: %v", err)
		return
	}
	if err := ensureManagedUser(su); err != nil {
		log.Printf("bootstrap user: %v", err)
		return
	}
	stateMu.Lock()
	paired := state.Paired
	stateMu.Unlock()
	if !paired {
		if err := pair(su); err != nil {
			log.Printf("pairing failed: %v (will retry)", err)
			go retryPairing()
		}
	}
	if state.HubPubKey == "" {
		// 已配对但缺 pubkey 的兜底
		if code, raw, err := api("GET", "/api/beszel/info", su, nil); err == nil && code == 200 {
			var info struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(raw, &info) == nil && info.Key != "" {
				stateMu.Lock()
				state.HubPubKey = info.Key
				saveState(state)
				stateMu.Unlock()
			}
		}
	}
	go startAgent()
	readyMu.Lock()
	ready = true
	readyMu.Unlock()
	log.Printf("gateway ready")
}

func retryPairing() {
	for {
		time.Sleep(30 * time.Second)
		stateMu.Lock()
		paired := state.Paired
		stateMu.Unlock()
		if paired {
			go startAgent()
			readyMu.Lock()
			ready = true
			readyMu.Unlock()
			return
		}
		su, err := superuserToken()
		if err != nil {
			continue
		}
		if err := pair(su); err == nil {
			go startAgent()
			readyMu.Lock()
			ready = true
			readyMu.Unlock()
			return
		}
	}
}

func isReady() bool {
	readyMu.RLock()
	defer readyMu.RUnlock()
	return ready
}

const landingHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Beszel</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;color:#444;background:#fafafa}.box{text-align:center}.spin{width:34px;height:34px;border:3px solid #ddd;border-top-color:#1055c9;border-radius:50%;margin:0 auto 14px;animation:s 0.8s linear infinite}@keyframes s{to{transform:rotate(360deg)}}</style>
</head><body><div class="box"><div class="spin"></div><div id="msg">正在打开 Beszel…</div></div>
<script>
(async () => {
  try {
    const r = await fetch('__P__/_gw/login');
    if (r.ok) {
      const d = await r.json();
      const auth = {token: d.token, record: d.record, model: d.record};
      localStorage.setItem('pocketbase_auth', JSON.stringify(auth));
    }
  } catch (e) {}
  document.cookie = 'bzboot=1; path=__P__/';
  location.replace('__P__/');
})();
</script></body></html>`

const bootHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Beszel</title><meta http-equiv="refresh" content="3">
<style>body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;color:#444;background:#fafafa}.box{text-align:center}.spin{width:34px;height:34px;border:3px solid #ddd;border-top-color:#1055c9;border-radius:50%;margin:0 auto 14px;animation:s 0.8s linear infinite}@keyframes s{to{transform:rotate(360deg)}}</style>
</head><body><div class="box"><div class="spin"></div><div>Beszel 正在启动，首次初始化约需半分钟…</div></div></body></html>`

func settingsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		_ = r.ParseForm()
		stateMu.Lock()
		state.AutoLogin = r.FormValue("autologin") == "on"
		saveState(state)
		stateMu.Unlock()
		http.Redirect(w, r, prefix+"/_gw/settings?saved=1", http.StatusSeeOther)
		return
	}
	stateMu.Lock()
	al := state.AutoLogin
	email, pass := state.UserEmail, state.UserPass
	stateMu.Unlock()
	checked := ""
	if al {
		checked = " checked"
	}
	saved := ""
	if r.URL.Query().Get("saved") == "1" {
		saved = `<div style="background:#e8f5e9;border:1px solid #a5d6a7;padding:8px 12px;border-radius:8px;margin-bottom:14px">已保存</div>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Beszel 系统监控 · 设置</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#fafafa;color:#333;margin:0;padding:32px 16px}.card{max-width:560px;margin:0 auto;background:#fff;border:1px solid #e5e5e5;border-radius:12px;padding:24px 28px}h1{font-size:19px;margin:0 0 6px}p{line-height:1.65;color:#555;font-size:14px}code{background:#f2f2f2;padding:2px 6px;border-radius:5px}label{font-size:14px}button{background:#1055c9;color:#fff;border:0;padding:8px 20px;border-radius:8px;font-size:14px;cursor:pointer;margin-top:10px}.row{margin:16px 0}hr{border:0;border-top:1px solid #eee;margin:20px 0}a{color:#1055c9}</style>
</head><body><div class="card"><h1>Beszel 系统监控 · 包装层设置</h1>
<p>这里只管“打开应用要不要登录”这一件事。告警、通知、添加其他机器、修改密码等全部设置都在 Beszel 面板内（右上角头像 → 设置），与原版一致。</p>%s
<form method="post"><div class="row"><label><input type="checkbox" name="autologin"%s> 打开应用自动登录（开箱即用，免注册免输密码）</label></div>
<p>关闭后，打开应用将使用 Beszel 原生登录页。本地账号如下，可直接登录，也可在面板里改密码或新建你自己的账号：</p>
<div class="row">账号：<code>%s</code><br>密码：<code>%s</code></div>
<button type="submit">保存</button></form>
<hr><p><a href="%s/">← 返回监控面板</a></p></div></body></html>`, saved, checked, email, pass, prefix)
}

func main() {
	log.SetPrefix("[beszel-gateway] ")
	dataDir = os.Getenv("BZ_DATA_DIR")
	appDir = os.Getenv("BZ_APP_DIR")
	port = os.Getenv("BZ_PORT")
	sockPath = os.Getenv("BZ_SOCK")
	prefix = os.Getenv("BZ_PREFIX")
	if dataDir == "" || appDir == "" || (port == "" && sockPath == "") {
		log.Fatalf("BZ_DATA_DIR / BZ_APP_DIR / (BZ_PORT or BZ_SOCK) required")
	}
	if sockPath != "" && prefix == "" {
		prefix = "/app/beszel-aio"
	}
	state = loadState()

	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", hubPort))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// hub(PocketBase) 给页面带 X-Frame-Options: SAMEORIGIN, 而飞牛桌面(:45167)
	// 与应用(:18092)不同端口=不同源, 在桌面窗口 iframe 里会被浏览器拦下,
	// 表现就是"拒绝连接"(标签页打开不受影响)。网关层剥掉防嵌套头, 本体不改。
	proxy.ModifyResponse = func(resp *http.Response) error {
		h := resp.Header
		h.Del("X-Frame-Options")
		h.Del("Cross-Origin-Opener-Policy")
		if prefix != "" {
			// 同源网关模式: 浏览器看到的地址带 /app/beszel-aio 前缀,
			// hub 的绝对路径跳转要补上前缀, 否则跳出应用路径
			if loc := h.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && !strings.HasPrefix(loc, prefix+"/") && loc != prefix {
				h.Set("Location", prefix+loc)
			}
			ct := h.Get("Content-Type")
			if h.Get("Content-Encoding") == "" && resp.ContentLength >= 0 && resp.ContentLength <= 16<<20 && rewritableType(ct) {
				raw, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err == nil {
					nb := rewriteBody(raw, ct)
					resp.Body = io.NopCloser(bytes.NewReader(nb))
					resp.ContentLength = int64(len(nb))
					h.Set("Content-Length", strconv.Itoa(len(nb)))
					h.Del("ETag")
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(raw))
				}
			}
		}
		if csp := h.Get("Content-Security-Policy"); strings.Contains(csp, "frame-ancestors") {
			var keep []string
			for _, d := range strings.Split(csp, ";") {
				if !strings.HasPrefix(strings.TrimSpace(d), "frame-ancestors") {
					keep = append(keep, d)
				}
			}
			if len(keep) == 0 {
				h.Del("Content-Security-Policy")
			} else {
				h.Set("Content-Security-Policy", strings.Join(keep, ";"))
			}
		}
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_gw/login", func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		al := state.AutoLogin
		stateMu.Unlock()
		if !al {
			http.Error(w, "autologin disabled", http.StatusForbidden)
			return
		}
		code, raw, err := api("POST", "/api/collections/users/auth-with-password", "", map[string]string{
			"identity": state.UserEmail, "password": state.UserPass,
		})
		if err != nil || code != 200 {
			http.Error(w, "login failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("/_gw/settings", settingsPage)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !isReady() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, bootHTML)
			return
		}
		stateMu.Lock()
		al := state.AutoLogin
		stateMu.Unlock()
		if al && r.URL.Path == "/" {
			if _, err := r.Cookie("bzboot"); err != nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, strings.ReplaceAll(landingHTML, "__P__", prefix))
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})

	go bootstrap()

	srv := &http.Server{Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		close(shutdownCh)
		procMu.Lock()
		ag, hb := agentCmd, hubCmd
		procMu.Unlock()
		stopProc(ag)
		stopProc(hb)
		os.Exit(0)
	}()
	if sockPath != "" {
		// fnOS 同源网关模式: 监听应用目录的 unix socket, 飞牛桌面经
		// /app/beszel-aio 前缀转发进来, TLS 由飞牛桌面终结,
		// 家里/外网/APP 远程都跟着桌面走, 不再依赖独立端口
		_ = os.Remove(sockPath)
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			log.Fatalf("listen sock: %v", err)
		}
		_ = os.Chmod(sockPath, 0660)
		log.Printf("listening on unix socket %s (prefix %s)", sockPath, prefix)
		if err := srv.Serve(ln); err != nil {
			log.Fatalf("serve: %v", err)
		}
		return
	}
	srv.Addr = ":" + port
	// 默认: 飞牛证书库里有可用证书就启用 HTTPS(飞牛桌面是 HTTPS,
	// 窗口内嵌页同为 HTTPS 才不会被浏览器按混合内容拦截成白屏);
	// 无证书时回退 HTTP(此时请用新标签页方式打开)。设 BZ_HTTPS=0 强制 HTTP。
	httpsMode := false
	if os.Getenv("BZ_HTTPS") != "0" {
		if _, _, ok := certFiles(); ok {
			httpsMode = true
		}
	}
	if httpsMode {
		srv.TLSConfig = &tls.Config{GetCertificate: getCertificate}
		log.Printf("listening on :%s (https, fnOS cert)", port)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("serve: %v", err)
		}
	} else {
		log.Printf("listening on :%s (http)", port)
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}
}

var _ = strings.TrimSpace
