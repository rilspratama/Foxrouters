// login_page.go — HTML template + error-injection helper for /login.
// Kept in a separate file to keep handlers.go focused on route logic.
package handlers

import "strings"

// loginPageHTML returns the login page with FoxRouters branding.
const loginPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>FoxRouters — Login</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;590;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #0d1117; --bg-panel: #161b22; --bg-elevated: #21262d;
  --text: #e6edf3; --text-tertiary: #6e7681; --text-quaternary: #484f58;
  --brand: #6366f1; --brand-hover: #818cf8;
  --border: #30363d; --border-bright: rgba(255,255,255,0.16);
  --red: #f85149; --red-subtle: rgba(248,81,73,0.12);
  --radius: 8px; --radius-lg: 12px;
  --font: 'Inter', -apple-system, sans-serif; --mono: 'JetBrains Mono', monospace;
  --shadow-modal: 0 8px 32px rgba(0,0,0,0.5);
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
  background: var(--brand); display: flex; align-items: center; justify-content: center;
  margin: 0 auto 20px; color: #fff; box-shadow: 0 4px 12px rgba(99,102,241,0.4);
}
.login-title { text-align: center; font-size: 20px; font-weight: 590; margin-bottom: 6px; }
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
.login-input:focus { outline: none; border-color: var(--brand); box-shadow: 0 0 0 3px rgba(99,102,241,0.15); }
.login-btn {
  width: 100%; padding: 11px; background: var(--brand); color: #fff; border: none;
  border-radius: var(--radius); font-size: 14px; font-weight: 590; cursor: pointer;
  font-family: var(--font); transition: background 150ms ease, box-shadow 150ms ease, transform 200ms ease;
  box-shadow: 0 1px 3px rgba(99,102,241,0.3);
}
.login-btn:hover { background: var(--brand-hover); box-shadow: 0 4px 12px rgba(99,102,241,0.4); transform: translateY(-1px); }
.login-btn:active { transform: translateY(0); }
.login-footer { text-align: center; margin-top: 20px; font-size: 11px; color: var(--text-quaternary); font-family: var(--mono); }
</style>
</head>
<body>
<div class="login-card">
  <div class="login-logo">
    <svg width="24" height="24" viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">
      <path d="M8 4L11 12L6 10L8 4Z" fill="currentColor"/>
      <path d="M24 4L21 12L26 10L24 4Z" fill="currentColor"/>
      <path d="M16 7C11 7 7 11 7 16C7 20 10 23 16 25C22 23 25 20 25 16C25 11 21 7 16 7Z" fill="currentColor"/>
      <path d="M12 15C13 14 14 14 16 14C18 14 19 14 20 15" stroke="rgba(255,255,255,0.9)" stroke-width="1.2" stroke-linecap="round" fill="none"/>
      <path d="M11 17C13 16 14.5 16 16 16C17.5 16 19 16 21 17" stroke="rgba(255,255,255,0.7)" stroke-width="1.2" stroke-linecap="round" fill="none"/>
      <circle cx="13" cy="13" r="1.2" fill="rgba(255,255,255,0.95)"/>
      <circle cx="19" cy="13" r="1.2" fill="rgba(255,255,255,0.95)"/>
      <circle cx="16" cy="19" r="1" fill="rgba(255,255,255,0.9)"/>
    </svg>
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
  <div class="login-footer">FoxRouters v5.11</div>
</div>
</body>
</html>`

// loginPageHTMLWithError returns the login page with an error message.
func loginPageHTMLWithError(msg string) string {
	return strings.Replace(loginPageHTML,
		`<div class="login-sub">Gateway Control Panel</div>`,
		`<div class="login-sub">Gateway Control Panel</div><div class="login-error">`+msg+`</div>`, 1)
}
