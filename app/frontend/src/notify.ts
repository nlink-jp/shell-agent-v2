// Native error dialog helper.
//
// window.alert() is not reliably rendered in the Wails v2 WKWebView, so
// every user-facing failure notice goes through the Go binding
// Bindings.ShowErrorDialog, which calls wailsRuntime.MessageDialog
// (ADR-0027 §2.4). Do not use window.alert() in this app.

export function showError(title: string, message: string): void {
    // Fire-and-forget: the dialog is informational and the binding does
    // not reject. Optional-chain so a not-yet-ready binding is a no-op.
    void window.go?.main?.Bindings?.ShowErrorDialog(title, message)
}
