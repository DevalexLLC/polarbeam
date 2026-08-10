// Runs as a classic render-blocking script in <head> so the resolved theme
// lands on <html> before first paint (no flash of the wrong scheme). It must
// stay an external file: the server's CSP is `default-src 'self'` with no
// inline allowance (internal/server/httpapi/static.go).
//
// Contract shared with src/theme.ts: localStorage 'polarbeam-theme' holds
// the *preference* ('light' | 'dark'; absent = system) while
// document.documentElement.dataset.theme always holds the *resolved* scheme.
;(function () {
  var pref = null
  try {
    pref = localStorage.getItem('polarbeam-theme')
  } catch {
    /* storage may be unavailable (private mode); fall through to system */
  }
  var dark = pref === 'dark' || (pref !== 'light' && matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.dataset.theme = dark ? 'dark' : 'light'
})()
