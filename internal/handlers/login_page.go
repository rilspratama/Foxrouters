// login_page.go — HTML template + error-injection helper for /login.
// Kept in a separate file to keep handlers.go focused on route logic.
package handlers

import (
	"html"
	"strings"
)

// loginPageHTML returns the login page with FoxRouters branding.
const loginPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>FoxRouters — Login</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Geist:wght@400;500;600;700&family=Geist+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #000000; --bg-panel: #171717; --bg-elevated: #1f1f1f;
  --text: #ffffff; --text-tertiary: #8c8c8c; --text-quaternary: #5c5c5c;
  --brand: #ffffff; --brand-hover: rgba(255,255,255,0.82); --brand-fg: #000000;
  --border: #2a2a2a; --border-bright: rgba(255,255,255,0.20);
  --red: #f87171; --red-subtle: rgba(248,113,113,0.12);
  --radius: 6px; --radius-lg: 10px;
  --font: 'Geist', 'Inter', -apple-system, sans-serif; --mono: 'Geist Mono', 'JetBrains Mono', monospace;
  --shadow-modal: 0 8px 32px rgba(0,0,0,0.7);
}
* { margin:0; padding:0; box-sizing:border-box; }
body {
  font-family: var(--font); background: var(--bg); color: var(--text);
  min-height: 100vh; display: flex; align-items: center; justify-content: center;
  -webkit-font-smoothing: antialiased;
}
.login-card {
  background: var(--bg-panel); border: 1px solid var(--border);
  border-radius: var(--radius-lg); padding: 40px; width: 90%; max-width: 400px;
  box-shadow: var(--shadow-modal);
}
.login-logo {
  width: 48px; height: 48px; border-radius: var(--radius);
  background: transparent; display: flex; align-items: center; justify-content: center;
  margin: 0 auto 20px; border: 1px solid rgba(255,255,255,0.1); overflow: hidden;
}
.login-title { text-align: center; font-size: 20px; font-weight: 600; margin-bottom: 6px; }
.login-sub { text-align: center; font-size: 13px; color: var(--text-tertiary); margin-bottom: 28px; }
.login-error {
  background: var(--red-subtle); color: var(--red); border: 1px solid rgba(248,81,73,0.3);
  border-radius: var(--radius); padding: 10px 14px; font-size: 13px; margin-bottom: 16px;
  text-align: center;
}
.login-field { margin-bottom: 16px; }
.login-label { font-size: 12px; color: var(--text-tertiary); display: block; margin-bottom: 6px; font-weight: 500; }
.login-input {
  width: 100%; padding: 10px 14px; background: var(--bg); border: 1px solid var(--border);
  border-radius: var(--radius); color: var(--text); font-family: var(--mono); font-size: 13px;
  transition: border-color 150ms ease;
}
.login-input:focus { outline: none; border-color: var(--brand); box-shadow: 0 0 0 3px rgba(255,255,255,0.15); }
.login-btn {
  width: 100%; padding: 11px; background: var(--brand); color: var(--brand-fg); border: none;
  border-radius: var(--radius); font-size: 14px; font-weight: 600; cursor: pointer;
  font-family: var(--font); transition: background 150ms ease, box-shadow 150ms ease, transform 200ms ease;
  box-shadow: 0 1px 3px rgba(255,255,255,0.12);
}
.login-btn:hover { background: var(--brand-hover); box-shadow: 0 4px 12px rgba(255,255,255,0.15); transform: translateY(-1px); }
.login-btn:active { transform: translateY(0); }
.login-footer { text-align: center; margin-top: 20px; font-size: 11px; color: var(--text-quaternary); font-family: var(--mono); }
</style>
</head>
<body>
<div class="login-card">
  <div class="login-logo">
    <img src="data:image/png;base64,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" width="40" height="40" alt="FoxRouters" style="display:block;border-radius:8px" />
  </div>
  <div class="login-title">FoxRouters</div>
  <div class="login-sub">Gateway Control Panel</div>
  <form method="POST" action="/login">
    <div class="login-field">
      <label class="login-label" for="key">Gateway API Key</label>
      <input class="login-input" type="password" id="key" name="key" placeholder="gw-..." autofocus required>
    </div>
    <button class="login-btn" type="submit">Sign In</button>
  </form>
  <div class="login-footer">FoxRouters v1.6.6</div>
</div>
</body>
</html>`

// loginPageHTMLWithError returns the login page with an error message.
// The message is HTML-escaped to prevent reflected XSS if any future code
// path passes user-controlled input to this function.
func loginPageHTMLWithError(msg string) string {
	return strings.Replace(loginPageHTML,
		`<div class="login-sub">Gateway Control Panel</div>`,
		`<div class="login-sub">Gateway Control Panel</div><div class="login-error">`+html.EscapeString(msg)+`</div>`, 1)
}
