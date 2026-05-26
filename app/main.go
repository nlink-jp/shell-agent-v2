package main

import (
	"context"
	"embed"
	"os"
	"os/signal"
	"syscall"

	"github.com/nlink-jp/shell-agent-v2/internal/config"
	"github.com/nlink-jp/shell-agent-v2/internal/pathfix"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var appIcon []byte

// repoURL is the source repository — shown in the About panel and
// opened by the Help → View on GitHub menu item.
const repoURL = "https://github.com/nlink-jp/shell-agent-v2"

// defaultWindowWidth / Height match the long-standing hardcoded
// window size; minWindowDimension floors absurd persisted values so a
// corrupt config can't open an unusable sliver of a window.
const (
	defaultWindowWidth  = 1024
	defaultWindowHeight = 768
	minWindowDimension  = 200
)

// windowSize derives the initial window dimensions from the saved
// config, falling back to the default and flooring sub-200px values
// (ADR-0024 Part A).
func windowSize(cfg *config.Config) (w, h int) {
	w, h = defaultWindowWidth, defaultWindowHeight
	if cfg != nil {
		if cfg.UI.Window.Width >= minWindowDimension {
			w = cfg.UI.Window.Width
		}
		if cfg.UI.Window.Height >= minWindowDimension {
			h = cfg.UI.Window.Height
		}
	}
	return w, h
}

func main() {
	// Finder/launchd-launched .app bundles inherit a minimal PATH
	// that excludes Homebrew and ~/bin, so anything we exec
	// (podman, docker, gh, user MCP servers) silently goes missing.
	// Augment PATH before any subprocess work begins.
	os.Setenv("PATH", pathfix.Augment(os.Getenv("PATH"), pathfix.Candidates(os.Getenv("HOME")), nil))

	// Load config up front so the saved window size sizes the window
	// at creation, rather than resizing after OnStartup completes
	// (ADR-0024 Part A). Wails v2 has no initial-position option, so
	// position is restored early in the now-fast startup() instead.
	// The same cfg is handed to the bindings so startup() doesn't
	// re-read the file.
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	winW, winH := windowSize(cfg)

	bindings := NewBindings(cfg)

	// Belt-and-braces shutdown for cases where the OS terminates
	// the process before Wails's OnShutdown fires (e.g.
	// `kill -TERM`). SIGKILL is uncatchable; the startup sweep in
	// maybeStartSandbox covers that path.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		bindings.shutdown(context.Background())
		os.Exit(0)
	}()

	// Application menu. The standard macOS structure (App / Edit /
	// Window) was missing entirely pre-v0.10 — the OS would render
	// a barebones default and there was no About panel exposing
	// the version. AppMenu picks up the mac.AboutInfo configured
	// below for the standard "About Shell Agent v2" item.
	//
	// Help → View on GitHub uses bindings.ctx (captured in
	// bindings.startup) rather than the wails.Run-scope context,
	// so the open happens against the live runtime once the app
	// is up.
	appMenu := menu.NewMenu()
	appMenu.Append(menu.AppMenu())
	appMenu.Append(menu.EditMenu())
	appMenu.Append(menu.WindowMenu())
	helpMenu := appMenu.AddSubmenu("Help")
	helpMenu.AddText("View on GitHub", nil, func(_ *menu.CallbackData) {
		if bindings.ctx != nil {
			wailsRuntime.BrowserOpenURL(bindings.ctx, repoURL)
		}
	})

	err = wails.Run(&options.App{
		Title:                    "Shell Agent v2",
		Width:                    winW,
		Height:                   winH,
		EnableDefaultContextMenu: true,
		Menu:                     appMenu,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        bindings.startup,
		OnShutdown:       bindings.shutdown,
		OnBeforeClose: func(ctx context.Context) (prevent bool) {
			if bindings.IsBusy() {
				dialog, err := wailsRuntime.MessageDialog(ctx, wailsRuntime.MessageDialogOptions{
					Type:          wailsRuntime.QuestionDialog,
					Title:         "Processing in progress",
					Message:       "The agent is currently busy. Quit anyway? Results may be lost.",
					DefaultButton: "No",
					Buttons:       []string{"Yes", "No"},
				})
				if err != nil || dialog == "No" {
					return true
				}
			}
			return false
		},
		Bind: []interface{}{
			bindings,
		},
		Mac: &mac.Options{
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                 true,
				FullSizeContent:           true,
			},
			// Window-level translucency requires private macOS APIs
			// (NSVisualEffectView et al) and pulls the desktop
			// through long messages / code blocks, which made the
			// chat pane hard to read. Keep the WebView opaque; CSS
			// inside still uses rgba on top of an opaque base.
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "Shell Agent v2",
				Message: "Version " + version + "\n\nmacOS-native LLM chat and agent.\n© 2026 nlink-jp\n" + repoURL,
				Icon:    appIcon,
			},
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
