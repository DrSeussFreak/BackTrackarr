import Cocoa
import WebKit

/// A thin native wrapper around a self-hosted BackTrackarr server.
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate {
    private var window: NSWindow!
    private var web: WKWebView!
    private var destinations: [ObjectIdentifier: URL] = [:]
    private var showingError = false

    private var server: String {
        get {
            UserDefaults.standard.string(forKey: "server")
                ?? (Bundle.main.object(forInfoDictionaryKey: "BTDefaultServer") as? String) ?? ""
        }
        set { UserDefaults.standard.set(newValue, forKey: "server") }
    }

    func applicationDidFinishLaunching(_ note: Notification) {
        buildMenu()
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        if #available(macOS 12.3, *) { config.preferences.isElementFullscreenEnabled = true }
        config.applicationNameForUserAgent = "BackTrackarrApp/1.0 Safari/605.1.15"
        web = WKWebView(frame: .zero, configuration: config)
        web.navigationDelegate = self
        web.uiDelegate = self
        web.allowsBackForwardNavigationGestures = true

        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1280, height: 860),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "BackTrackarr"
        window.minSize = NSSize(width: 420, height: 500)
        window.backgroundColor = NSColor(calibratedRed: 0.08, green: 0.08, blue: 0.075, alpha: 1)
        window.contentView = web
        window.delegate = self
        window.center()
        window.setFrameAutosaveName("BackTrackarrMain")
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)

        if server.isEmpty { changeServer(nil) } else { loadHome() }
    }

    func applicationShouldHandleReopen(_ app: NSApplication, hasVisibleWindows: Bool) -> Bool {
        if !hasVisibleWindows { window.makeKeyAndOrderFront(nil) }
        return true
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        sender.orderOut(nil) // keeps running uploads alive; Quit ends the app
        return false
    }

    private func loadHome() {
        guard let url = URL(string: server) else { changeServer(nil); return }
        web.load(URLRequest(url: url))
    }

    private func sameServer(_ url: URL?) -> Bool {
        guard let url, let home = URL(string: server), let host = home.host else { return false }
        return host.caseInsensitiveCompare(url.host ?? "") == .orderedSame
            && home.scheme?.lowercased() == url.scheme?.lowercased()
            && effectivePort(home) == effectivePort(url)
    }

    private func effectivePort(_ url: URL) -> Int {
        url.port ?? (url.scheme?.lowercased() == "https" ? 443 : 80)
    }

    private func openOutside(_ url: URL) {
        // Pages can't launch other apps through custom links.
        if ["http", "https"].contains(url.scheme?.lowercased() ?? "") { NSWorkspace.shared.open(url) }
    }

    private static func normalize(_ raw: String) -> String? {
        var v = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        while v.hasSuffix("/") { v.removeLast() }
        if v.isEmpty { return nil }
        if !v.contains("://") { v = "https://" + v }
        guard let u = URL(string: v), u.host != nil, ["http", "https"].contains(u.scheme?.lowercased() ?? "") else { return nil }
        return v
    }

    // MARK: Menu actions

    @objc func changeServer(_ sender: Any?) {
        let alert = NSAlert()
        alert.messageText = "BackTrackarr server"
        alert.informativeText = "Enter the address you use to open BackTrackarr."
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 340, height: 24))
        field.placeholderString = "https://backtrackarr.example.com"
        field.stringValue = server
        alert.accessoryView = field
        alert.addButton(withTitle: "Connect")
        alert.addButton(withTitle: server.isEmpty ? "Quit" : "Cancel")
        alert.window.initialFirstResponder = field
        while true {
            guard alert.runModal() == .alertFirstButtonReturn else {
                if server.isEmpty { NSApp.terminate(nil) }
                return
            }
            if let address = Self.normalize(field.stringValue) {
                server = address
                loadHome()
                return
            }
            alert.informativeText = "Enter a full address, such as https://example.com"
        }
    }

    @objc func goHome(_ sender: Any?) { loadHome() }
    @objc func reloadPage(_ sender: Any?) {
        if web.url == nil { loadHome() } else { web.reload() }
    }
    @objc func goBackPage(_ sender: Any?) { web.goBack() }
    @objc func goForwardPage(_ sender: Any?) { web.goForward() }

    private func buildMenu() {
        let main = NSMenu()
        func add(_ title: String, _ items: [NSMenuItem]) -> NSMenu {
            let holder = NSMenuItem()
            let menu = NSMenu(title: title)
            items.forEach(menu.addItem)
            holder.submenu = menu
            main.addItem(holder)
            return menu
        }
        func item(_ title: String, _ action: Selector, _ key: String, _ mods: NSEvent.ModifierFlags = .command) -> NSMenuItem {
            let i = NSMenuItem(title: title, action: action, keyEquivalent: key)
            i.keyEquivalentModifierMask = mods
            return i
        }
        _ = add("BackTrackarr", [
            item("About BackTrackarr", #selector(NSApplication.orderFrontStandardAboutPanel(_:)), ""),
            .separator(),
            item("Change Server…", #selector(changeServer(_:)), ","),
            .separator(),
            item("Hide BackTrackarr", #selector(NSApplication.hide(_:)), "h"),
            item("Quit BackTrackarr", #selector(NSApplication.terminate(_:)), "q"),
        ])
        _ = add("Edit", [
            item("Undo", Selector(("undo:")), "z"),
            item("Redo", Selector(("redo:")), "Z", [.command, .shift]),
            .separator(),
            item("Cut", #selector(NSText.cut(_:)), "x"),
            item("Copy", #selector(NSText.copy(_:)), "c"),
            item("Paste", #selector(NSText.paste(_:)), "v"),
            item("Select All", #selector(NSText.selectAll(_:)), "a"),
        ])
        _ = add("View", [
            item("Reload", #selector(reloadPage(_:)), "r"),
            item("Home", #selector(goHome(_:)), "H", [.command, .shift]),
            .separator(),
            item("Back", #selector(goBackPage(_:)), "["),
            item("Forward", #selector(goForwardPage(_:)), "]"),
            .separator(),
            item("Enter Full Screen", #selector(NSWindow.toggleFullScreen(_:)), "f", [.command, .control]),
        ])
        let windowMenu = add("Window", [
            item("Minimize", #selector(NSWindow.performMiniaturize(_:)), "m"),
            item("Close", #selector(NSWindow.performClose(_:)), "w"),
        ])
        NSApp.windowsMenu = windowMenu
        NSApp.mainMenu = main
    }

    // MARK: Navigation

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url else { decisionHandler(.cancel); return }
        if action.shouldPerformDownload && sameServer(url) { decisionHandler(.download); return }
        if action.targetFrame?.isMainFrame == false || sameServer(url) || url.scheme == "about" {
            decisionHandler(.allow)
            return
        }
        openOutside(url)
        decisionHandler(.cancel)
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        let http = response.response as? HTTPURLResponse
        let disposition = http?.value(forHTTPHeaderField: "Content-Disposition")?.lowercased() ?? ""
        if response.isForMainFrame, let code = http?.statusCode, (502...504).contains(code) {
            decisionHandler(.cancel)
            showFailure("Your BackTrackarr server is restarting or offline. Try again in a moment.")
            return
        }
        decisionHandler(disposition.hasPrefix("attachment") || !response.canShowMIMEType ? .download : .allow)
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) { download.delegate = self }
    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) { download.delegate = self }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation nav: WKNavigation!, withError error: Error) { handle(error) }
    func webView(_ webView: WKWebView, didFail nav: WKNavigation!, withError error: Error) { handle(error) }

    private func handle(_ error: Error) {
        let e = error as NSError
        // Cancelled loads and pages turned into downloads are not failures.
        if e.domain == NSURLErrorDomain && e.code == NSURLErrorCancelled { return }
        if e.domain == "WebKitErrorDomain" && (e.code == 102 || e.code == 101) { return }
        showFailure("Can't reach your BackTrackarr server. Check your connection and the server address.")
    }

    private func showFailure(_ message: String) {
        guard !showingError else { return }
        showingError = true
        DispatchQueue.main.async { [self] in
            let alert = NSAlert()
            alert.messageText = "BackTrackarr"
            alert.informativeText = message
            alert.addButton(withTitle: "Try Again")
            alert.addButton(withTitle: "Change Server")
            alert.addButton(withTitle: "Cancel")
            let choice = alert.runModal()
            showingError = false
            switch choice {
            case .alertFirstButtonReturn: reloadPage(nil)
            case .alertSecondButtonReturn: changeServer(nil)
            default: break
            }
        }
    }

    // MARK: Page dialogs, new windows and file picking

    func webView(_ webView: WKWebView, createWebViewWith config: WKWebViewConfiguration, for action: WKNavigationAction,
                 windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = action.request.url {
            if sameServer(url) { webView.load(action.request) } else { openOutside(url) }
        }
        return nil
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping () -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        alert.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (Bool) -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        alert.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn) }
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (String?) -> Void) {
        let alert = NSAlert()
        alert.messageText = prompt
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 320, height: 24))
        field.stringValue = defaultText ?? ""
        alert.accessoryView = field
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        alert.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn ? field.stringValue : nil) }
    }

    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.canChooseFiles = true
        panel.beginSheetModal(for: window) { completionHandler($0 == .OK ? panel.urls : nil) }
    }

    // MARK: Downloads

    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String,
                  completionHandler: @escaping (URL?) -> Void) {
        let folder = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask)[0]
        let safe = (suggestedFilename as NSString).lastPathComponent
        let name = safe.isEmpty || safe == "." || safe == ".." ? "BackTrackarr-download" : safe
        let base = (name as NSString).deletingPathExtension
        let ext = (name as NSString).pathExtension
        var target = folder.appendingPathComponent(name)
        var n = 2
        while FileManager.default.fileExists(atPath: target.path) {
            target = folder.appendingPathComponent(ext.isEmpty ? "\(base) \(n)" : "\(base) \(n).\(ext)")
            n += 1
        }
        destinations[ObjectIdentifier(download)] = target
        completionHandler(target)
    }

    func downloadDidFinish(_ download: WKDownload) {
        if let url = destinations.removeValue(forKey: ObjectIdentifier(download)) {
            NSWorkspace.shared.activateFileViewerSelecting([url])
        }
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        destinations.removeValue(forKey: ObjectIdentifier(download))
        let alert = NSAlert()
        alert.messageText = "Download failed"
        alert.informativeText = error.localizedDescription
        alert.runModal()
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
