package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gwa "github.com/go-webauthn/webauthn/webauthn"
)

// ---- Domain types ----

type Segment struct {
	Category string     `json:"category"`
	Segment  [2]float64 `json:"segment"`
	UUID     string     `json:"UUID"`
}

type PlaylistEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Interval struct {
	Start float64
	End   float64
}

// ---- SSE broadcaster ----

type sseEvent struct {
	name string // named event; empty = default "message"
	data []byte // JSON payload
}

type broadcaster struct {
	mu      sync.Mutex
	clients map[chan sseEvent]struct{}
	history []sseEvent
}

var bc = &broadcaster{clients: make(map[chan sseEvent]struct{})}

func (b *broadcaster) subscribe() chan sseEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan sseEvent, 512)
	for _, e := range b.history {
		ch <- e
	}
	b.clients[ch] = struct{}{}
	return ch
}

func (b *broadcaster) unsubscribe(ch chan sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, ch)
}

func (b *broadcaster) send(e sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.history = append(b.history, e)
	for ch := range b.clients {
		select {
		case ch <- e:
		default:
		}
	}
}

func (b *broadcaster) sendLog(level, text string) {
	type logEntry struct {
		Text  string `json:"text"`
		Level string `json:"level"`
	}
	data, _ := json.Marshal(logEntry{Text: text, Level: level})
	b.send(sseEvent{data: data})
}

func (b *broadcaster) sendNamed(name string, v any) {
	data, _ := json.Marshal(v)
	b.send(sseEvent{name: name, data: data})
}

func (b *broadcaster) finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		close(ch)
	}
	b.clients = make(map[chan sseEvent]struct{})
}

// ---- Failure tracking ----

type FailedEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

var (
	failuresMu sync.Mutex
	failures   []FailedEntry
)

func addFailure(e FailedEntry) {
	failuresMu.Lock()
	failures = append(failures, e)
	failuresMu.Unlock()
	bc.sendNamed("failure", e)
}

func removeFailure(id string) {
	failuresMu.Lock()
	defer failuresMu.Unlock()
	for i, f := range failures {
		if f.ID == id {
			failures = append(failures[:i], failures[i+1:]...)
			return
		}
	}
}

func getFailures() []FailedEntry {
	failuresMu.Lock()
	defer failuresMu.Unlock()
	out := make([]FailedEntry, len(failures))
	copy(out, failures)
	return out
}

// ---- Global config (needed by retry handler) ----

var cfg struct {
	passkeyFile string
	appCfgFile  string
}

// ---- App config (persisted, editable via web UI) ----

type AppConfig struct {
	URL        string   `json:"url"`
	Output     string   `json:"output"`
	Categories []string `json:"categories"`
	Cookies    string   `json:"cookies"`
	Interval   string   `json:"interval"`
}

var (
	appCfgMu sync.Mutex
	appCfg   AppConfig
)

func getAppCfg() AppConfig {
	appCfgMu.Lock()
	defer appCfgMu.Unlock()
	return appCfg
}

func setAppCfg(c AppConfig) error {
	if err := os.MkdirAll(filepath.Dir(cfg.appCfgFile), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(cfg.appCfgFile, data, 0o644); err != nil {
		return err
	}
	appCfgMu.Lock()
	appCfg = c
	appCfgMu.Unlock()
	return nil
}

func loadAppCfgFile() {
	data, err := os.ReadFile(cfg.appCfgFile)
	if err != nil {
		return
	}
	var c AppConfig
	if json.Unmarshal(data, &c) == nil {
		appCfgMu.Lock()
		appCfg = c
		appCfgMu.Unlock()
	}
}

// ---- Run state ----

var (
	runMu    sync.Mutex
	runActive bool
)

func tryStartRun() bool {
	runMu.Lock()
	defer runMu.Unlock()
	if runActive {
		return false
	}
	runActive = true
	return true
}

func endRun() {
	runMu.Lock()
	runActive = false
	runMu.Unlock()
	bc.sendNamed("run_done", struct{}{})
}

func isRunning() bool {
	runMu.Lock()
	defer runMu.Unlock()
	return runActive
}

// ---- Passkey / WebAuthn auth ----

var wa *gwa.WebAuthn

// passkeyUser is the single owner account, persisted to disk.
type passkeyUser struct {
	ID          []byte           `json:"id"`
	Name        string           `json:"name"`
	Credentials []gwa.Credential `json:"credentials"`
}

func (u *passkeyUser) WebAuthnID() []byte                    { return u.ID }
func (u *passkeyUser) WebAuthnName() string                  { return u.Name }
func (u *passkeyUser) WebAuthnDisplayName() string           { return "autodl-music" }
func (u *passkeyUser) WebAuthnCredentials() []gwa.Credential { return u.Credentials }

func loadPasskeyUser() (*passkeyUser, error) {
	data, err := os.ReadFile(cfg.passkeyFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u passkeyUser
	return &u, json.Unmarshal(data, &u)
}

func savePasskeyUser(u *passkeyUser) error {
	data, _ := json.Marshal(u)
	return os.WriteFile(cfg.passkeyFile, data, 0o600)
}

// auth sessions (HttpOnly cookie → expiry)
var (
	authSessionsMu sync.Mutex
	authSessions   = map[string]time.Time{}
)

func newAuthToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	authSessionsMu.Lock()
	authSessions[token] = time.Now().Add(7 * 24 * time.Hour)
	authSessionsMu.Unlock()
	return token
}

func isAuthenticated(r *http.Request) bool {
	c, err := r.Cookie("auth")
	if err != nil {
		return false
	}
	authSessionsMu.Lock()
	exp, ok := authSessions[c.Value]
	authSessionsMu.Unlock()
	return ok && time.Now().Before(exp)
}

func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAuthenticated(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		h(w, r)
	}
}

// WebAuthn challenge sessions (short-lived, between begin/finish)
var (
	waChallengeMu sync.Mutex
	waChallenges  = map[string]*gwa.SessionData{}
)

func storeChallenge(data *gwa.SessionData) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	waChallengeMu.Lock()
	waChallenges[id] = data
	waChallengeMu.Unlock()
	return id
}

func popChallenge(id string) (*gwa.SessionData, bool) {
	waChallengeMu.Lock()
	defer waChallengeMu.Unlock()
	d, ok := waChallenges[id]
	if ok {
		delete(waChallenges, id)
	}
	return d, ok
}

func setAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "auth", Value: newAuthToken(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: 7 * 24 * 3600,
	})
}

// ---- Auth HTTP handlers ----

const loginPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>autodl-music — sign in</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
:root{--red:#ff3c3c;--bg:#0a0a0a;--surface:#0d0d0d;--border:#1a1a1a;--muted:#2e2e2e;--text:#d8d8d8;--dim:#555}
body{background:var(--bg);color:var(--text);font-family:'Courier New',monospace;min-height:100vh;display:flex;align-items:center;justify-content:center}
body::before{content:'';position:fixed;inset:0;background-image:radial-gradient(circle,rgba(255,255,255,.035) 1px,transparent 1px);background-size:20px 20px;pointer-events:none;z-index:0}
.card{position:relative;z-index:1;width:320px;padding:48px 40px;border:1px solid var(--border);background:var(--surface);display:flex;flex-direction:column;align-items:center;gap:28px}
.logo{display:flex;align-items:center;gap:12px}
.dot{width:10px;height:10px;border-radius:50%;background:var(--red)}
h1{font-size:13px;letter-spacing:.32em;text-transform:uppercase;font-weight:400}
.hint{font-size:11px;color:var(--dim);letter-spacing:.06em;text-align:center;line-height:1.7}
.btn{width:100%;background:none;border:1px solid var(--muted);color:var(--text);font-family:inherit;font-size:11px;letter-spacing:.18em;text-transform:uppercase;padding:11px 0;cursor:pointer;transition:border-color .2s,color .2s}
.btn:hover:not(:disabled){border-color:var(--red);color:var(--red)}
.btn:disabled{opacity:.35;cursor:default}
.err{font-size:11px;color:var(--red);text-align:center;letter-spacing:.05em;min-height:14px}
</style>
</head>
<body>
<div class="card">
  <div class="logo"><div class="dot"></div><h1>autodl&#x2011;music</h1></div>
  <p class="hint" id="hint">checking&hellip;</p>
  <button class="btn" id="btn" disabled onclick="auth()"></button>
  <p class="err" id="err"></p>
</div>
<script>
let registered=false;
const b64url=b=>btoa(String.fromCharCode(...new Uint8Array(b))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=/g,'');
const b64dec=s=>{s=s.replace(/-/g,'+').replace(/_/g,'/');return Uint8Array.from(atob(s),c=>c.charCodeAt(0)).buffer};

async function init(){
  const d=await(await fetch('/auth/status')).json();
  registered=d.registered;
  document.getElementById('hint').textContent=registered?'Use your passkey to sign in':'No passkey registered yet';
  const btn=document.getElementById('btn');
  btn.textContent=registered?'sign in with passkey':'register passkey';
  btn.disabled=false;
}

async function auth(){
  const btn=document.getElementById('btn'),err=document.getElementById('err');
  btn.disabled=true;err.textContent='';
  try{registered?await login():await register();}
  catch(e){err.textContent=e.message||'error';btn.disabled=false;}
}

async function register(){
  const opts=await(await fetch('/auth/register/begin',{method:'POST'})).json();
  opts.publicKey.challenge=b64dec(opts.publicKey.challenge);
  opts.publicKey.user.id=b64dec(opts.publicKey.user.id);
  const cred=await navigator.credentials.create(opts);
  const res=await fetch('/auth/register/finish',{method:'POST',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({id:cred.id,rawId:b64url(cred.rawId),type:cred.type,
      response:{attestationObject:b64url(cred.response.attestationObject),
                clientDataJSON:b64url(cred.response.clientDataJSON)}})});
  if(!res.ok)throw new Error(await res.text());
  location.href='/';
}

async function login(){
  const opts=await(await fetch('/auth/login/begin',{method:'POST'})).json();
  opts.publicKey.challenge=b64dec(opts.publicKey.challenge);
  (opts.publicKey.allowCredentials||[]).forEach(c=>c.id=b64dec(c.id));
  const a=await navigator.credentials.get(opts);
  const res=await fetch('/auth/login/finish',{method:'POST',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({id:a.id,rawId:b64url(a.rawId),type:a.type,
      response:{authenticatorData:b64url(a.response.authenticatorData),
                clientDataJSON:b64url(a.response.clientDataJSON),
                signature:b64url(a.response.signature),
                userHandle:a.response.userHandle?b64url(a.response.userHandle):null}})});
  if(!res.ok)throw new Error(await res.text());
  location.href='/';
}

init();
</script>
</body>
</html>`

func serveLogin(w http.ResponseWriter, r *http.Request) {
	if isAuthenticated(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginPage))
}

func serveAuthStatus(w http.ResponseWriter, _ *http.Request) {
	u, _ := loadPasskeyUser()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"registered": u != nil && len(u.Credentials) > 0})
}

func serveRegisterBegin(w http.ResponseWriter, _ *http.Request) {
	u, _ := loadPasskeyUser()
	if u == nil {
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		u = &passkeyUser{ID: id, Name: "owner"}
		_ = savePasskeyUser(u)
	}
	options, sessionData, err := wa.BeginRegistration(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "wa_reg", Value: storeChallenge(sessionData), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(options)
}

func serveRegisterFinish(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("wa_reg")
	if err != nil {
		http.Error(w, "missing challenge cookie", http.StatusBadRequest)
		return
	}
	sessionData, ok := popChallenge(cookie.Value)
	if !ok {
		http.Error(w, "expired challenge", http.StatusBadRequest)
		return
	}
	u, _ := loadPasskeyUser()
	if u == nil {
		http.Error(w, "user not initialised", http.StatusBadRequest)
		return
	}
	cred, err := wa.FinishRegistration(u, *sessionData, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.Credentials = append(u.Credentials, *cred)
	if err := savePasskeyUser(u); err != nil {
		http.Error(w, "failed to save credential", http.StatusInternalServerError)
		return
	}
	setAuthCookie(w)
	w.WriteHeader(http.StatusOK)
}

func serveLoginBegin(w http.ResponseWriter, _ *http.Request) {
	u, err := loadPasskeyUser()
	if err != nil || u == nil || len(u.Credentials) == 0 {
		http.Error(w, "not registered", http.StatusBadRequest)
		return
	}
	options, sessionData, err := wa.BeginLogin(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "wa_auth", Value: storeChallenge(sessionData), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(options)
}

func serveLoginFinish(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("wa_auth")
	if err != nil {
		http.Error(w, "missing challenge cookie", http.StatusBadRequest)
		return
	}
	sessionData, ok := popChallenge(cookie.Value)
	if !ok {
		http.Error(w, "expired challenge", http.StatusBadRequest)
		return
	}
	u, err := loadPasskeyUser()
	if err != nil || u == nil {
		http.Error(w, "not registered", http.StatusBadRequest)
		return
	}
	cred, err := wa.FinishLogin(u, *sessionData, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	for i, c := range u.Credentials {
		if bytes.Equal(c.ID, cred.ID) {
			u.Credentials[i].Authenticator.SignCount = cred.Authenticator.SignCount
			break
		}
	}
	_ = savePasskeyUser(u)
	setAuthCookie(w)
	w.WriteHeader(http.StatusOK)
}

func serveLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("auth"); err == nil {
		authSessionsMu.Lock()
		delete(authSessions, c.Value)
		authSessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "auth", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}


// ---- Logger ----

var webMode bool

func logInfo(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Print(msg)
	if webMode {
		bc.sendLog("info", strings.TrimRight(msg, "\n"))
	}
}

func logError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprint(os.Stderr, msg)
	if webMode {
		bc.sendLog("error", strings.TrimRight(msg, "\n"))
	}
}

// lineWriter tees writes to target and broadcasts each newline-delimited line.
type lineWriter struct {
	target io.Writer
	level  string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n, err := w.target.Write(p)
	if webMode {
		w.buf = append(w.buf, p...)
		for {
			idx := bytes.IndexByte(w.buf, '\n')
			if idx < 0 {
				break
			}
			line := strings.TrimRight(string(w.buf[:idx]), "\r")
			w.buf = w.buf[idx+1:]
			if line != "" {
				bc.sendLog(w.level, line)
			}
		}
	}
	return n, err
}

// ---- HTTP handlers ----

const htmlPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>autodl-music</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
:root{--red:#ff3c3c;--bg:#0a0a0a;--surface:#0d0d0d;--border:#1a1a1a;--muted:#2e2e2e;--text:#d8d8d8;--dim:#555;--dot-color:rgba(255,255,255,.035)}
.light{--bg:#f5f5f5;--surface:#ebebeb;--border:#d4d4d4;--muted:#b0b0b0;--text:#1a1a1a;--dim:#888;--dot-color:rgba(0,0,0,.045)}
body{background:var(--bg);color:var(--text);font-family:'Courier New',monospace;min-height:100vh;transition:background .25s,color .25s}
body::before{content:'';position:fixed;inset:0;background-image:radial-gradient(circle,var(--dot-color) 1px,transparent 1px);background-size:20px 20px;pointer-events:none;z-index:0}
.wrap{position:relative;z-index:1;max-width:900px;margin:0 auto;padding:40px 24px}
header{display:flex;align-items:center;gap:14px;padding-bottom:24px;border-bottom:1px solid var(--border);margin-bottom:28px}
.dot{width:10px;height:10px;border-radius:50%;background:var(--red);flex-shrink:0;transition:background .4s}
.dot.pulse{animation:pulse 1.4s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.25}}
h1{font-size:13px;letter-spacing:.32em;text-transform:uppercase;font-weight:400}
#badge{font-size:10px;letter-spacing:.14em;text-transform:uppercase;padding:3px 10px;border:1px solid var(--muted);color:var(--muted);transition:all .3s}
#badge.live{border-color:var(--red);color:var(--red)}
#badge.done{border-color:#4ade80;color:#4ade80}
#badge.off{border-color:var(--muted);color:var(--muted)}
#theme-btn{margin-left:auto;background:none;border:1px solid var(--border);color:var(--muted);font-family:inherit;font-size:10px;letter-spacing:.14em;text-transform:uppercase;padding:3px 10px;cursor:pointer;transition:border-color .2s,color .2s}
#theme-btn:hover{border-color:var(--muted);color:var(--text)}
.terminal{background:var(--surface);border:1px solid var(--border);padding:20px 24px;min-height:500px;max-height:70vh;overflow-y:auto;transition:background .25s,border-color .25s}
.line{font-size:12px;line-height:1.85;white-space:pre-wrap;word-break:break-all}
.ts{color:var(--muted);margin-right:10px;user-select:none;font-size:11px}
.line.error .msg{color:var(--red)}
.line.success .msg{color:#16a34a}
.line.info .msg{color:var(--text)}
.line.sub .msg{color:var(--dim)}
.empty{color:var(--muted);font-size:11px;letter-spacing:.1em}
footer{margin-top:16px;display:flex;justify-content:space-between;font-size:10px;color:var(--muted);letter-spacing:.12em;text-transform:uppercase}
/* failures table */
#failures-section{margin-top:36px;display:none}
.section-label{font-size:10px;letter-spacing:.22em;text-transform:uppercase;color:var(--muted);margin-bottom:12px}
table{width:100%;border-collapse:collapse}
thead th{text-align:left;font-weight:400;font-size:10px;letter-spacing:.14em;text-transform:uppercase;color:var(--muted);padding:6px 12px;border-bottom:1px solid var(--border)}
tbody tr{border-bottom:1px solid var(--border);transition:background .15s}
tbody tr:hover{background:var(--surface)}
td{padding:10px 12px;font-size:12px;vertical-align:middle}
.td-id{color:var(--dim);font-size:11px;width:110px}
.td-title{word-break:break-word}
.td-status{width:80px;font-size:10px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted)}
.td-actions{width:160px;white-space:nowrap}
.btn{background:none;border:1px solid var(--border);color:var(--muted);font-family:inherit;font-size:10px;letter-spacing:.1em;text-transform:uppercase;padding:3px 9px;cursor:pointer;transition:all .2s;margin-right:6px}
.btn:hover{border-color:var(--text);color:var(--text)}
.btn.retry:hover{border-color:var(--red);color:var(--red)}
.btn.remove:hover{border-color:var(--muted);color:var(--dim)}
.btn:disabled{opacity:.35;cursor:default;pointer-events:none}
::-webkit-scrollbar{width:3px}::-webkit-scrollbar-track{background:transparent}::-webkit-scrollbar-thumb{background:var(--muted)}
/* settings panel */
#settings{border:1px solid var(--border);margin-bottom:24px}
.settings-hd{display:flex;align-items:center;justify-content:space-between;padding:9px 16px;cursor:pointer;user-select:none}
.settings-hd:hover .settings-label{color:var(--text)}
.settings-label{font-size:10px;letter-spacing:.2em;text-transform:uppercase;color:var(--muted);transition:color .2s}
.settings-arrow{font-size:10px;color:var(--muted);transition:transform .2s}
.settings-arrow.open{transform:rotate(180deg)}
.settings-body{border-top:1px solid var(--border);padding:16px;display:none}
.settings-body.open{display:block}
.srow{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:12px}
.srow.full{grid-template-columns:1fr}
.field label{display:block;font-size:10px;letter-spacing:.14em;text-transform:uppercase;color:var(--muted);margin-bottom:5px}
.field input{width:100%;background:none;border:1px solid var(--border);color:var(--text);font-family:inherit;font-size:12px;padding:6px 10px;outline:none;transition:border-color .2s}
.field input:focus{border-color:var(--muted)}
.settings-actions{display:flex;gap:8px;justify-content:flex-end;margin-top:16px}
.btn-cfg{background:none;border:1px solid var(--border);color:var(--muted);font-family:inherit;font-size:10px;letter-spacing:.14em;text-transform:uppercase;padding:5px 14px;cursor:pointer;transition:all .2s}
.btn-cfg:hover{border-color:var(--muted);color:var(--text)}
#start-btn{border-color:var(--red);color:var(--red)}
#start-btn:hover:not(:disabled){background:var(--red);color:#000}
#start-btn:disabled{opacity:.35;cursor:default;pointer-events:none}
#start-btn.running{border-color:var(--muted);color:var(--muted);background:none}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div class="dot pulse" id="dot"></div>
    <h1>autodl&#x2011;music</h1>
    <div id="badge" class="live">&#9679; live</div>
    <button id="theme-btn" onclick="toggleTheme()">light</button>
    <a href="/logout" style="font-size:10px;letter-spacing:.12em;text-transform:uppercase;color:var(--muted);text-decoration:none;padding:3px 0" onmouseover="this.style.color='var(--text)'" onmouseout="this.style.color='var(--muted)'">logout</a>
  </header>
  <section id="settings">
    <div class="settings-hd" onclick="toggleSettings()">
      <span class="settings-label">configuration</span>
      <span class="settings-arrow" id="settings-arrow">▼</span>
    </div>
    <div class="settings-body" id="settings-body">
      <div class="srow full"><div class="field">
        <label>Playlist URL</label>
        <input type="text" id="cfg-url" placeholder="https://youtube.com/playlist?list=...">
      </div></div>
      <div class="srow">
        <div class="field">
          <label>Output directory</label>
          <input type="text" id="cfg-output" placeholder="./music">
        </div>
        <div class="field">
          <label>Interval (e.g. 1h, 30m)</label>
          <input type="text" id="cfg-interval" placeholder="disabled">
        </div>
      </div>
      <div class="srow">
        <div class="field">
          <label>SponsorBlock categories</label>
          <input type="text" id="cfg-categories" placeholder="sponsor,outro,selfpromo,...">
        </div>
        <div class="field">
          <label>Cookies file path</label>
          <input type="text" id="cfg-cookies" placeholder="/config/cookies.txt">
        </div>
      </div>
      <div class="settings-actions">
        <button class="btn-cfg" onclick="saveConfig()">save</button>
        <button class="btn-cfg" id="start-btn" onclick="startRun()">&#9654; start</button>
      </div>
    </div>
  </section>
  <div class="terminal" id="logs"><div class="empty">waiting for output&hellip;</div></div>
  <footer>
    <span>yt-dlp &bull; sponsorblock &bull; ffmpeg</span>
    <span id="count">0 lines</span>
  </footer>

  <section id="failures-section">
    <div class="section-label">Failed downloads</div>
    <table>
      <thead>
        <tr>
          <th>ID</th>
          <th>Title</th>
          <th>Status</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody id="failures-body"></tbody>
    </table>
  </section>
</div>
<script>
const logs=document.getElementById('logs');
const badge=document.getElementById('badge');
const dot=document.getElementById('dot');
const countEl=document.getElementById('count');
const themeBtn=document.getElementById('theme-btn');
const failuresSection=document.getElementById('failures-section');
const failuresBody=document.getElementById('failures-body');
let n=0,empty=true,dark=true;

function toggleTheme(){
  dark=!dark;
  document.body.classList.toggle('light',!dark);
  themeBtn.textContent=dark?'light':'dark';
  localStorage.setItem('theme',dark?'dark':'light');
}
if(localStorage.getItem('theme')==='light'){toggleTheme()}

function ts(){return new Date().toISOString().slice(11,19)}
function classify(t){
  if(/\berror\b|\bfailed\b|\bwarning\b/i.test(t)) return 'error';
  if(/saved:|done\.|succeeded/i.test(t)) return 'success';
  if(/^\s+/.test(t)) return 'sub';
  return 'info';
}
function esc(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')}

function append(text,level){
  if(empty){logs.innerHTML='';empty=false}
  const d=document.createElement('div');
  d.className='line '+(level||classify(text));
  d.innerHTML='<span class="ts">'+ts()+'</span><span class="msg">'+esc(text)+'</span>';
  logs.appendChild(d);
  logs.scrollTop=logs.scrollHeight;
  countEl.textContent=(++n)+' line'+(n===1?'':'s');
}

// ---- failures table ----

function rowId(id){return'row-'+id.replace(/[^a-z0-9]/gi,'_')}

function addFailureRow(id,title){
  failuresSection.style.display='block';
  if(document.getElementById(rowId(id))) return;
  const tr=document.createElement('tr');
  tr.id=rowId(id);
  tr.innerHTML=
    '<td class="td-id">'+esc(id)+'</td>'+
    '<td class="td-title">'+esc(title||id)+'</td>'+
    '<td class="td-status" id="st-'+rowId(id)+'">failed</td>'+
    '<td class="td-actions">'+
      '<button class="btn retry" onclick="retry(\''+esc(id)+'\',\''+esc(title||id)+'\',this)">retry</button>'+
      '<button class="btn remove" onclick="remove(\''+esc(id)+'\',this)">remove</button>'+
    '</td>';
  failuresBody.appendChild(tr);
}

function setStatus(id,text){
  const el=document.getElementById('st-'+rowId(id));
  if(el) el.textContent=text;
}

function removeRow(id){
  const tr=document.getElementById(rowId(id));
  if(tr) tr.remove();
  if(!failuresBody.children.length) failuresSection.style.display='none';
}

function retry(id,title,btn){
  btn.disabled=true;
  const removBtn=btn.nextElementSibling;
  if(removBtn) removBtn.disabled=true;
  setStatus(id,'retrying…');
  fetch('/retry?id='+encodeURIComponent(id),{method:'POST'}).catch(()=>{
    setStatus(id,'error');
    btn.disabled=false;
    if(removBtn) removBtn.disabled=false;
  });
}

function remove(id,btn){
  btn.disabled=true;
  fetch('/remove?id='+encodeURIComponent(id),{method:'POST'}).then(()=>removeRow(id)).catch(()=>{btn.disabled=false});
}

// ---- SSE ----

const es=new EventSource('/logs');
es.onopen=()=>{badge.className='live';badge.innerHTML='&#9679; live'};
es.onmessage=(e)=>{const d=JSON.parse(e.data);append(d.text,d.level)};
es.addEventListener('done',()=>{
  badge.className='done';badge.textContent='✓ done';
  dot.classList.remove('pulse');dot.style.background='#4ade80';
});
es.addEventListener('failure',(e)=>{
  const d=JSON.parse(e.data);
  addFailureRow(d.id,d.title);
});
es.addEventListener('retry_start',(e)=>{
  const d=JSON.parse(e.data);
  setStatus(d.id,'retrying…');
});
es.addEventListener('retry_ok',(e)=>{
  const d=JSON.parse(e.data);
  removeRow(d.id);
});
es.addEventListener('retry_fail',(e)=>{
  const d=JSON.parse(e.data);
  setStatus(d.id,'failed');
  const tr=document.getElementById(rowId(d.id));
  if(tr){
    tr.querySelectorAll('.btn').forEach(b=>b.disabled=false);
  }
});
es.onerror=()=>{
  badge.className='off';badge.textContent='✗ offline';
  dot.classList.remove('pulse');dot.style.background='#2e2e2e';
};
es.addEventListener('run_done',()=>{
  setRunning(false);
  badge.className='done';badge.textContent='✓ done';
  dot.classList.remove('pulse');dot.style.background='#4ade80';
});

// ---- settings panel ----
let settingsOpen=false;
function toggleSettings(){
  settingsOpen=!settingsOpen;
  document.getElementById('settings-body').classList.toggle('open',settingsOpen);
  document.getElementById('settings-arrow').classList.toggle('open',settingsOpen);
}

async function loadConfig(){
  try{
    const c=await(await fetch('/api/config')).json();
    document.getElementById('cfg-url').value=c.url||'';
    document.getElementById('cfg-output').value=c.output||'';
    document.getElementById('cfg-categories').value=(c.categories||[]).join(',');
    document.getElementById('cfg-cookies').value=c.cookies||'';
    document.getElementById('cfg-interval').value=c.interval||'';
    if(!c.url)toggleSettings();
  }catch(e){}
}

async function saveConfig(){
  const cats=document.getElementById('cfg-categories').value.split(',').map(s=>s.trim()).filter(Boolean);
  await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({
      url:document.getElementById('cfg-url').value.trim(),
      output:document.getElementById('cfg-output').value.trim()||'./music',
      categories:cats,
      cookies:document.getElementById('cfg-cookies').value.trim(),
      interval:document.getElementById('cfg-interval').value.trim(),
    })});
}

function setRunning(running){
  const btn=document.getElementById('start-btn');
  btn.disabled=running;
  btn.classList.toggle('running',running);
  btn.innerHTML=running?'&#9679; running':'&#9654; start';
  if(running){
    badge.className='live';badge.innerHTML='&#9679; live';
    dot.classList.add('pulse');dot.style.background='var(--red)';
  }
}

async function startRun(){
  await saveConfig();
  const r=await fetch('/api/start',{method:'POST'});
  if(r.status===409){append('Already running','error');return;}
  if(!r.ok){append('Failed to start: '+(await r.text()),'error');return;}
  setRunning(true);
}

// init
loadConfig();
fetch('/api/status').then(r=>r.json()).then(d=>{if(d.running)setRunning(true)}).catch(()=>{});
</script>
</body>
</html>`

func serveHome(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(htmlPage))
}

func serveLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := bc.subscribe()
	defer bc.unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-ch:
			if !open {
				fmt.Fprintf(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if e.name != "" {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", e.data)
			}
			flusher.Flush()
		}
	}
}

func serveFailures(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(getFailures())
}

func serveRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")

	failuresMu.Lock()
	var found *FailedEntry
	for i := range failures {
		if failures[i].ID == id {
			cp := failures[i]
			found = &cp
			break
		}
	}
	failuresMu.Unlock()

	if found == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	entry := *found
	go func() {
		logInfo("\n[retry] %s (%s)\n", entry.Title, entry.ID)
		bc.sendNamed("retry_start", map[string]string{"id": entry.ID})

		c := getAppCfg()
		retryTmp := filepath.Join(c.Output, ".tmp", "retry_"+entry.ID)
		_ = os.MkdirAll(retryTmp, 0o755)
		defer os.RemoveAll(retryTmp)

		err := processVideo(
			PlaylistEntry{ID: entry.ID, Title: entry.Title},
			c.Output, retryTmp, c.Cookies, c.Categories,
		)
		if err != nil {
			logError("  Retry failed: %v\n", err)
			bc.sendNamed("retry_fail", map[string]string{"id": entry.ID})
		} else {
			removeFailure(entry.ID)
			bc.sendNamed("retry_ok", map[string]string{"id": entry.ID})
		}
	}()

	w.WriteHeader(http.StatusAccepted)
}

func serveRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	removeFailure(r.URL.Query().Get("id"))
	w.WriteHeader(http.StatusNoContent)
}

// ---- SponsorBlock / yt-dlp logic ----

func getSponsorBlockSegments(videoID string, categories []string) ([]Segment, error) {
	catJSON, _ := json.Marshal(categories)
	apiURL := fmt.Sprintf("https://sponsor.ajay.app/api/skipSegments?videoID=%s&categories=%s",
		videoID, url.QueryEscape(string(catJSON)))

	resp, err := http.Get(apiURL) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sponsorblock returned %d", resp.StatusCode)
	}

	var segments []Segment
	if err := json.NewDecoder(resp.Body).Decode(&segments); err != nil {
		return nil, err
	}
	return segments, nil
}

func ytdlpArgs(base []string, cookiesFile string) []string {
	if cookiesFile != "" {
		base = append(base, "--cookies", cookiesFile)
	}
	return base
}

func getPlaylistEntries(playlistURL, cookiesFile string) ([]PlaylistEntry, error) {
	args := ytdlpArgs([]string{
		"--flat-playlist", "-j", "--no-warnings",
		"--extractor-args", "youtube:player_client=ios,mweb",
		playlistURL,
	}, cookiesFile)
	cmd := exec.Command("yt-dlp", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &lineWriter{target: os.Stderr, level: "error"}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var entries []PlaylistEntry
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var entry PlaylistEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil && entry.ID != "" {
			entries = append(entries, entry)
		}
	}
	_ = cmd.Wait()
	return entries, nil
}

func downloadAudio(videoID, outputDir, cookiesFile string) (string, error) {
	outputTemplate := filepath.Join(outputDir, "%(id)s.%(ext)s")
	args := ytdlpArgs([]string{
		"-x", "--audio-format", "mp3",
		"--audio-quality", "0",
		"--embed-metadata",
		"--embed-thumbnail",
		"--convert-thumbnails", "jpg",
		"-o", outputTemplate,
		"--no-playlist",
		"--no-warnings",
		fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID),
	}, cookiesFile)
	cmd := exec.Command("yt-dlp", args...)
	cmd.Stdout = &lineWriter{target: os.Stdout, level: "info"}
	cmd.Stderr = &lineWriter{target: os.Stderr, level: "error"}
	if err := cmd.Run(); err != nil {
		return "", err
	}

	pattern := filepath.Join(outputDir, videoID+".mp3")
	if _, err := os.Stat(pattern); err == nil {
		return pattern, nil
	}
	matches, err := filepath.Glob(filepath.Join(outputDir, videoID+".*"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("downloaded file not found for %s", videoID)
	}
	return matches[0], nil
}

func mergeIntervals(intervals []Interval) []Interval {
	if len(intervals) == 0 {
		return nil
	}
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i].Start < intervals[j].Start
	})
	merged := []Interval{intervals[0]}
	for _, iv := range intervals[1:] {
		last := &merged[len(merged)-1]
		if iv.Start <= last.End {
			if iv.End > last.End {
				last.End = iv.End
			}
		} else {
			merged = append(merged, iv)
		}
	}
	return merged
}

func invertIntervals(removeIntervals []Interval, duration float64) []Interval {
	merged := mergeIntervals(removeIntervals)
	var keep []Interval
	pos := 0.0
	for _, iv := range merged {
		if iv.Start > pos+0.01 {
			keep = append(keep, Interval{Start: pos, End: iv.Start})
		}
		pos = iv.End
	}
	if duration-pos > 0.01 {
		keep = append(keep, Interval{Start: pos, End: duration})
	}
	return keep
}

func getAudioDuration(filePath string) (float64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		filePath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var result struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(result.Format.Duration, 64)
}

func cutSegments(inputFile string, segments []Segment, outputFile string) error {
	duration, err := getAudioDuration(inputFile)
	if err != nil {
		return fmt.Errorf("failed to get duration: %w", err)
	}

	var removeIntervals []Interval
	for _, seg := range segments {
		removeIntervals = append(removeIntervals, Interval{
			Start: seg.Segment[0],
			End:   seg.Segment[1],
		})
	}

	keepIntervals := invertIntervals(removeIntervals, duration)
	if len(keepIntervals) == 0 {
		return fmt.Errorf("no content to keep after removing segments")
	}
	if len(keepIntervals) == 1 && keepIntervals[0].Start == 0 && keepIntervals[0].End == duration {
		return os.Rename(inputFile, outputFile)
	}

	var parts []string
	for i, iv := range keepIntervals {
		parts = append(parts,
			fmt.Sprintf("[0:a]atrim=start=%.6f:end=%.6f,asetpts=PTS-STARTPTS[a%d]", iv.Start, iv.End, i))
	}
	var inputs strings.Builder
	for i := range keepIntervals {
		fmt.Fprintf(&inputs, "[a%d]", i)
	}
	parts = append(parts,
		fmt.Sprintf("%sconcat=n=%d:v=0:a=1[out]", inputs.String(), len(keepIntervals)))

	filter := strings.Join(parts, ";")

	cmd := exec.Command("ffmpeg", "-y",
		"-i", inputFile,
		"-filter_complex", filter,
		"-map", "[out]",
		outputFile,
	)
	cmd.Stdout = &lineWriter{target: os.Stdout, level: "info"}
	cmd.Stderr = &lineWriter{target: os.Stderr, level: "error"}
	return cmd.Run()
}

// ---- Core processing ----

// sanitizeFilename replaces characters that are invalid in filenames.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\x00':
			b.WriteRune('-')
		default:
			if r >= 0x20 {
				b.WriteRune(r)
			}
		}
	}
	name := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if len(name) > 200 {
		name = strings.TrimSpace(name[:200])
	}
	if name == "" {
		return "_"
	}
	return name
}

// finalFilename returns the destination filename: "Title [id].mp3"
func finalFilename(entry PlaylistEntry) string {
	title := entry.Title
	if title == "" {
		title = entry.ID
	}
	return sanitizeFilename(title) + " [" + entry.ID + "].mp3"
}

// processVideo downloads and processes a single video. Returns an error on failure.
func processVideo(entry PlaylistEntry, outputDir, tmpDir, cookiesFile string, cats []string) error {
	finalPath := filepath.Join(outputDir, finalFilename(entry))
	if _, err := os.Stat(finalPath); err == nil {
		logInfo("  Already processed, skipping.\n")
		return nil
	}

	segments, err := getSponsorBlockSegments(entry.ID, cats)
	if err != nil {
		logInfo("  Warning: SponsorBlock error: %v\n", err)
	}
	logInfo("  SponsorBlock: %d segment(s) to remove\n", len(segments))

	downloadedFile, err := downloadAudio(entry.ID, tmpDir, cookiesFile)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	if len(segments) > 0 {
		logInfo("  Cutting segments...\n")
		if err := cutSegments(downloadedFile, segments, finalPath); err != nil {
			logError("  Error cutting segments: %v — saving uncut version\n", err)
			if err2 := os.Rename(downloadedFile, finalPath); err2 != nil {
				return fmt.Errorf("save failed: %w", err2)
			}
		} else {
			os.Remove(downloadedFile)
		}
	} else {
		if err := os.Rename(downloadedFile, finalPath); err != nil {
			return fmt.Errorf("save failed: %w", err)
		}
	}
	logInfo("  Saved: %s\n", finalPath)
	return nil
}

func run() {
	if !tryStartRun() {
		logInfo("A run is already in progress.\n")
		return
	}
	defer endRun()

	c := getAppCfg()
	if c.URL == "" {
		logError("No playlist URL configured. Set it in the Settings panel.\n")
		return
	}
	if c.Output == "" {
		c.Output = "./music"
	}

	tmpDir := filepath.Join(c.Output, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		logError("Failed to create tmp dir: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	logInfo("Fetching playlist entries...\n")
	entries, err := getPlaylistEntries(c.URL, c.Cookies)
	if err != nil {
		logError("Failed to get playlist: %v\n", err)
		return
	}
	logInfo("Found %d video(s)\n", len(entries))

	var failCount int
	for i, entry := range entries {
		logInfo("\n[%d/%d] %s (%s)\n", i+1, len(entries), entry.Title, entry.ID)

		if err := processVideo(entry, c.Output, tmpDir, c.Cookies, c.Categories); err != nil {
			logError("  Error: %v\n", err)
			addFailure(FailedEntry{ID: entry.ID, Title: entry.Title})
			failCount++
		}
	}

	logInfo("\nDone. %d/%d succeeded.\n", len(entries)-failCount, len(entries))
	if failCount > 0 {
		logError("%d download(s) failed — see the table below.\n", failCount)
	}
}

// ---- App config / run API handlers ----

func serveGetConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(getAppCfg())
}

func serveSetConfig(w http.ResponseWriter, r *http.Request) {
	var c AppConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := setAppCfg(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serveStart(w http.ResponseWriter, _ *http.Request) {
	if isRunning() {
		http.Error(w, "already running", http.StatusConflict)
		return
	}
	go run()
	w.WriteHeader(http.StatusAccepted)
}

func serveRunStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"running": isRunning()})
}

// ---- Entry point ----

func main() {
	urlFlag := flag.String("url", "", "Playlist URL (can also be set via web UI)")
	outputFlag := flag.String("output", "", "Output directory (overrides config file)")
	categoriesFlag := flag.String("categories", "", "Comma-separated SponsorBlock categories (overrides config file)")
	cookiesFlag := flag.String("cookies", "", "Cookies file path (overrides config file)")
	intervalFlag := flag.String("interval", "", "Re-run interval, e.g. 1h, 30m (overrides config file)")
	web := flag.Bool("web", false, "Start web UI")
	port := flag.Int("port", 8080, "Web UI port")
	host := flag.String("host", "localhost", "Hostname for WebAuthn RPID")
	passkeyFileFlag := flag.String("passkey", "passkey.json", "Path to passkey credential file")
	appCfgFileFlag := flag.String("config", "autodl-music.json", "Path to persistent app config file")
	flag.Parse()

	cfg.passkeyFile = *passkeyFileFlag
	cfg.appCfgFile = *appCfgFileFlag

	// Load persisted config, then apply any CLI overrides.
	loadAppCfgFile()
	c := getAppCfg()
	if *urlFlag != "" {
		c.URL = *urlFlag
	}
	if *outputFlag != "" {
		c.Output = *outputFlag
	}
	if *categoriesFlag != "" {
		cats := strings.Split(*categoriesFlag, ",")
		for i, v := range cats {
			cats[i] = strings.TrimSpace(v)
		}
		c.Categories = cats
	}
	if *cookiesFlag != "" {
		c.Cookies = *cookiesFlag
	}
	if *intervalFlag != "" {
		c.Interval = *intervalFlag
	}
	if c.Output == "" {
		c.Output = "./music"
	}
	if len(c.Categories) == 0 {
		c.Categories = strings.Split("sponsor,outro,selfpromo,interaction,music_offtopic", ",")
	}
	appCfgMu.Lock()
	appCfg = c
	appCfgMu.Unlock()

	if *web {
		webMode = true

		var err error
		wa, err = gwa.New(&gwa.Config{
			RPDisplayName: "autodl-music",
			RPID:          *host,
			RPOrigins:     []string{fmt.Sprintf("http://%s:%d", *host, *port)},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "WebAuthn init error: %v\n", err)
			os.Exit(1)
		}

		mux := http.NewServeMux()
		// public auth routes
		mux.HandleFunc("/login", serveLogin)
		mux.HandleFunc("/logout", serveLogout)
		mux.HandleFunc("/auth/status", serveAuthStatus)
		mux.HandleFunc("/auth/register/begin", serveRegisterBegin)
		mux.HandleFunc("/auth/register/finish", serveRegisterFinish)
		mux.HandleFunc("/auth/login/begin", serveLoginBegin)
		mux.HandleFunc("/auth/login/finish", serveLoginFinish)
		// protected routes
		mux.HandleFunc("/", requireAuth(serveHome))
		mux.HandleFunc("/logs", requireAuth(serveLogs))
		mux.HandleFunc("/failures", requireAuth(serveFailures))
		mux.HandleFunc("/retry", requireAuth(serveRetry))
		mux.HandleFunc("/remove", requireAuth(serveRemove))
		mux.HandleFunc("/api/config", requireAuth(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				serveGetConfig(w, r)
			} else {
				serveSetConfig(w, r)
			}
		}))
		mux.HandleFunc("/api/start", requireAuth(serveStart))
		mux.HandleFunc("/api/status", requireAuth(serveRunStatus))

		fmt.Printf("Web UI: http://localhost:%d\n", *port)
		go func() {
			if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), mux); err != nil {
				fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
			}
		}()

		// Auto-start if URL already known, then keep scheduling.
		go func() {
			if getAppCfg().URL != "" {
				run()
			}
			for {
				time.Sleep(30 * time.Second)
				cur := getAppCfg()
				if cur.Interval == "" || isRunning() {
					continue
				}
				d, err := time.ParseDuration(cur.Interval)
				if err != nil {
					continue
				}
				// Simple approach: run once per interval after the last check.
				// A proper ticker would require tracking last-run time; keep it simple.
				time.Sleep(d - 30*time.Second)
				if !isRunning() {
					logInfo("\nScheduled re-run (every %s)...\n", cur.Interval)
					run()
				}
			}
		}()

		select {} // keep server alive
	} else {
		// CLI mode: require a URL.
		if getAppCfg().URL == "" {
			fmt.Fprintln(os.Stderr, "Usage: autodl-music -url <playlist-url> [options]")
			fmt.Fprintln(os.Stderr, "       autodl-music -web  (configure via browser)")
			os.Exit(1)
		}
		run()
	}
}
