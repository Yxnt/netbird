//go:build !(linux && 386)
// +build !linux !386

// skipping linux 32 bits build and tests
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"github.com/cenkalti/backoff/v4"
	"github.com/getlantern/systray"
	log "github.com/sirupsen/logrus"
	"github.com/skratchdot/open-golang/open"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/netbirdio/netbird/client/internal"
	"github.com/netbirdio/netbird/client/proto"
	"github.com/netbirdio/netbird/client/system"
	"github.com/netbirdio/netbird/version"
)

const (
	defaultFailTimeout = 3 * time.Second
	failFastTimeout    = time.Second
)

func main() {
	var daemonAddr string

	defaultDaemonAddr := "unix:///var/run/netbird.sock"
	if runtime.GOOS == "windows" {
		defaultDaemonAddr = "tcp://127.0.0.1:41731"
	}

	flag.StringVar(
		&daemonAddr, "daemon-addr",
		defaultDaemonAddr,
		"Daemon service address to serve CLI requests [unix|tcp]://[path|host:port]")

	var showSettings bool
	flag.BoolVar(&showSettings, "settings", false, "run settings windows")

	flag.Parse()

	a := app.New()
	a.SetIcon(fyne.NewStaticResource("netbird", iconDisconnectedPNG))

	client := newServiceClient(daemonAddr, a, showSettings)
	if showSettings {
		a.Run()
	} else {
		if err := checkPIDFile(); err != nil {
			fmt.Println(err)
			return
		}
		systray.Run(client.onTrayReady, client.onTrayExit)
	}
}

//go:embed connected.ico
var iconConnectedICO []byte

//go:embed connected.png
var iconConnectedPNG []byte

//go:embed disconnected.ico
var iconDisconnectedICO []byte

//go:embed disconnected.png
var iconDisconnectedPNG []byte

type serviceClient struct {
	ctx  context.Context
	addr string
	conn proto.DaemonServiceClient

	icConnected    []byte
	icDisconnected []byte

	// systray menu items
	mStatus     *systray.MenuItem
	mUp         *systray.MenuItem
	mDown       *systray.MenuItem
	mAdminPanel *systray.MenuItem
	mSettings   *systray.MenuItem
	mQuit       *systray.MenuItem

	// application with main windows.
	app          fyne.App
	wSettings    fyne.Window
	showSettings bool

	// input elements for settings form
	iMngURL       *widget.Entry
	iAdminURL     *widget.Entry
	iConfigFile   *widget.Entry
	iLogFile      *widget.Entry
	iPreSharedKey *widget.Entry

	// observable settings over corresponding iMngURL and iPreSharedKey values.
	managementURL string
	preSharedKey  string
	adminURL      string
}

// newServiceClient instance constructor
//
// This constructor also builds the UI elements for the settings window.
func newServiceClient(addr string, a fyne.App, showSettings bool) *serviceClient {
	s := &serviceClient{
		ctx:  context.Background(),
		addr: addr,
		app:  a,

		showSettings: showSettings,
	}

	if runtime.GOOS == "windows" {
		s.icConnected = iconConnectedICO
		s.icDisconnected = iconDisconnectedICO
	} else {
		s.icConnected = iconConnectedPNG
		s.icDisconnected = iconDisconnectedPNG
	}

	if showSettings {
		s.showUIElements()
		return s
	}

	return s
}

func (s *serviceClient) showUIElements() {
	// add settings window UI elements.
	s.wSettings = s.app.NewWindow("NetBird Settings")
	s.iMngURL = widget.NewEntry()
	s.iAdminURL = widget.NewEntry()
	s.iConfigFile = widget.NewEntry()
	s.iConfigFile.Disable()
	s.iLogFile = widget.NewEntry()
	s.iLogFile.Disable()
	s.iPreSharedKey = widget.NewPasswordEntry()
	s.wSettings.SetContent(s.getSettingsForm())
	s.wSettings.Resize(fyne.NewSize(600, 100))

	s.getSrvConfig()

	s.wSettings.Show()
}

// getSettingsForm to embed it into settings window.
func (s *serviceClient) getSettingsForm() *widget.Form {
	return &widget.Form{
		Items: []*widget.FormItem{
			{Text: "Management URL", Widget: s.iMngURL},
			{Text: "Admin URL", Widget: s.iAdminURL},
			{Text: "Pre-shared Key", Widget: s.iPreSharedKey},
			{Text: "Config File", Widget: s.iConfigFile},
			{Text: "Log File", Widget: s.iLogFile},
		},
		SubmitText: "Save",
		OnSubmit: func() {
			if s.iPreSharedKey.Text != "" && s.iPreSharedKey.Text != "**********" {
				// validate preSharedKey if it added
				if _, err := wgtypes.ParseKey(s.iPreSharedKey.Text); err != nil {
					dialog.ShowError(fmt.Errorf("Invalid Pre-shared Key Value"), s.wSettings)
					return
				}
			}

			defer s.wSettings.Close()
			// if management URL or Pre-shared key changed, we try to re-login with new settings.
			if s.managementURL != s.iMngURL.Text || s.preSharedKey != s.iPreSharedKey.Text ||
				s.adminURL != s.iAdminURL.Text {

				s.managementURL = s.iMngURL.Text
				s.preSharedKey = s.iPreSharedKey.Text
				s.adminURL = s.iAdminURL.Text

				client, err := s.getSrvClient(failFastTimeout)
				if err != nil {
					log.Errorf("get daemon client: %v", err)
					return
				}

				_, err = client.Login(s.ctx, &proto.LoginRequest{
					ManagementUrl:        s.iMngURL.Text,
					AdminURL:             s.iAdminURL.Text,
					PreSharedKey:         s.iPreSharedKey.Text,
					IsLinuxDesktopClient: runtime.GOOS == "linux",
				})
				if err != nil {
					log.Errorf("login to management URL: %v", err)
					return
				}

				_, err = client.Up(s.ctx, &proto.UpRequest{})
				if err != nil {
					log.Errorf("login to management URL: %v", err)
					return
				}

			}
			s.wSettings.Close()
		},
		OnCancel: func() {
			s.wSettings.Close()
		},
	}
}

func (s *serviceClient) login() error {
	conn, err := s.getSrvClient(defaultFailTimeout)
	if err != nil {
		log.Errorf("get client: %v", err)
		return err
	}

	loginResp, err := conn.Login(s.ctx, &proto.LoginRequest{
		IsLinuxDesktopClient: runtime.GOOS == "linux",
	})
	if err != nil {
		log.Errorf("login to management URL with: %v", err)
		return err
	}

	if loginResp.NeedsSSOLogin {
		err = open.Run(loginResp.VerificationURIComplete)
		if err != nil {
			log.Errorf("opening the verification uri in the browser failed: %v", err)
			return err
		}

		_, err = conn.WaitSSOLogin(s.ctx, &proto.WaitSSOLoginRequest{UserCode: loginResp.UserCode})
		if err != nil {
			log.Errorf("waiting sso login failed with: %v", err)
			return err
		}
	}

	return nil
}

func (s *serviceClient) menuUpClick() error {
	conn, err := s.getSrvClient(defaultFailTimeout)
	if err != nil {
		log.Errorf("get client: %v", err)
		return err
	}

	err = s.login()
	if err != nil {
		log.Errorf("login failed with: %v", err)
		return err
	}

	status, err := conn.Status(s.ctx, &proto.StatusRequest{})
	if err != nil {
		log.Errorf("get service status: %v", err)
		return err
	}

	if status.Status == string(internal.StatusConnected) {
		log.Warnf("already connected")
		return err
	}

	if _, err := s.conn.Up(s.ctx, &proto.UpRequest{}); err != nil {
		log.Errorf("up service: %v", err)
		return err
	}
	return nil
}

func (s *serviceClient) menuDownClick() error {
	conn, err := s.getSrvClient(defaultFailTimeout)
	if err != nil {
		log.Errorf("get client: %v", err)
		return err
	}

	status, err := conn.Status(s.ctx, &proto.StatusRequest{})
	if err != nil {
		log.Errorf("get service status: %v", err)
		return err
	}

	if status.Status != string(internal.StatusConnected) {
		log.Warnf("already down")
		return nil
	}

	if _, err := s.conn.Down(s.ctx, &proto.DownRequest{}); err != nil {
		log.Errorf("down service: %v", err)
		return err
	}

	return nil
}

func (s *serviceClient) updateStatus() error {
	conn, err := s.getSrvClient(defaultFailTimeout)
	if err != nil {
		return err
	}
	err = backoff.Retry(func() error {
		status, err := conn.Status(s.ctx, &proto.StatusRequest{})
		if err != nil {
			log.Errorf("get service status: %v", err)
			return err
		}

		if status.Status == string(internal.StatusConnected) && !s.mUp.Disabled() {
			systray.SetIcon(s.icConnected)
			systray.SetTooltip("NetBird (Connected)")
			s.mStatus.SetTitle("Connected")
			s.mUp.Disable()
			s.mDown.Enable()
		} else if status.Status != string(internal.StatusConnected) && s.mUp.Disabled() {
			systray.SetIcon(s.icDisconnected)
			systray.SetTooltip("NetBird (Disconnected)")
			s.mStatus.SetTitle("Disconnected")
			s.mDown.Disable()
			s.mUp.Enable()
		}
		return nil
	}, &backoff.ExponentialBackOff{
		InitialInterval:     time.Second,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          backoff.DefaultMultiplier,
		MaxInterval:         300 * time.Millisecond,
		MaxElapsedTime:      2 * time.Second,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	})

	if err != nil {
		return err
	}

	return nil
}

func (s *serviceClient) onTrayReady() {
	systray.SetIcon(s.icDisconnected)
	systray.SetTooltip("NetBird")

	// setup systray menu items
	s.mStatus = systray.AddMenuItem("Disconnected", "Disconnected")
	s.mStatus.Disable()
	systray.AddSeparator()
	s.mUp = systray.AddMenuItem("Connect", "Connect")
	s.mDown = systray.AddMenuItem("Disconnect", "Disconnect")
	s.mDown.Disable()
	s.mAdminPanel = systray.AddMenuItem("Admin Panel", "Wiretrustee Admin Panel")
	systray.AddSeparator()
	s.mSettings = systray.AddMenuItem("Settings", "Settings of the application")
	systray.AddSeparator()
	v := systray.AddMenuItem("v"+version.NetbirdVersion(), "Client Version: "+version.NetbirdVersion())
	v.Disable()
	systray.AddSeparator()
	s.mQuit = systray.AddMenuItem("Quit", "Quit the client app")

	go func() {
		s.getSrvConfig()
		for {
			err := s.updateStatus()
			if err != nil {
				log.Errorf("error while updating status: %v", err)
			}
			time.Sleep(2 * time.Second)
		}
	}()

	go func() {
		var err error
		for {
			select {
			case <-s.mAdminPanel.ClickedCh:
				err = open.Run(s.adminURL)
			case <-s.mUp.ClickedCh:
				go func() {
					err := s.menuUpClick()
					if err != nil {
						return
					}
				}()
			case <-s.mDown.ClickedCh:
				go func() {
					err := s.menuDownClick()
					if err != nil {
						return
					}
				}()
			case <-s.mSettings.ClickedCh:
				s.mSettings.Disable()
				go func() {
					defer s.mSettings.Enable()
					proc, err := os.Executable()
					if err != nil {
						log.Errorf("show settings: %v", err)
						return
					}

					cmd := exec.Command(proc, "--settings=true")
					out, err := cmd.CombinedOutput()
					if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
						log.Errorf("start settings UI: %v, %s", err, string(out))
						return
					}
					if len(out) != 0 {
						log.Info("settings change:", string(out))
					}

					// update config in systray when settings windows closed
					s.getSrvConfig()
				}()
			case <-s.mQuit.ClickedCh:
				systray.Quit()
				return
			}
			if err != nil {
				log.Errorf("process connection: %v", err)
			}
		}
	}()
}

func (s *serviceClient) onTrayExit() {}

// getSrvClient connection to the service.
func (s *serviceClient) getSrvClient(timeout time.Duration) (proto.DaemonServiceClient, error) {
	if s.conn != nil {
		return s.conn, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := grpc.DialContext(
		ctx,
		strings.TrimPrefix(s.addr, "tcp://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithUserAgent(system.GetDesktopUIUserAgent()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial service: %w", err)
	}

	s.conn = proto.NewDaemonServiceClient(conn)
	return s.conn, nil
}

// getSrvConfig from the service to show it in the settings window.
func (s *serviceClient) getSrvConfig() {
	s.managementURL = "https://api.wiretrustee.com:33073"
	s.adminURL = "https://app.netbird.io"

	conn, err := s.getSrvClient(failFastTimeout)
	if err != nil {
		log.Errorf("get client: %v", err)
		return
	}

	cfg, err := conn.GetConfig(s.ctx, &proto.GetConfigRequest{})
	if err != nil {
		log.Errorf("get config settings from server: %v", err)
		return
	}

	if cfg.ManagementUrl != "" {
		s.managementURL = cfg.ManagementUrl
	}
	if cfg.AdminURL != "" {
		s.adminURL = cfg.AdminURL
	}
	s.preSharedKey = cfg.PreSharedKey

	if s.showSettings {
		s.iMngURL.SetText(s.managementURL)
		s.iAdminURL.SetText(s.adminURL)
		s.iConfigFile.SetText(cfg.ConfigFile)
		s.iLogFile.SetText(cfg.LogFile)
		s.iPreSharedKey.SetText(cfg.PreSharedKey)
	}
}

// checkPIDFile exists and return error, or write new.
func checkPIDFile() error {
	pidFile := path.Join(os.TempDir(), "wiretrustee-ui.pid")
	if piddata, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(string(piddata)); err == nil {
			if process, err := os.FindProcess(pid); err == nil {
				if err := process.Signal(syscall.Signal(0)); err == nil {
					return fmt.Errorf("process already exists: %d", pid)
				}
			}
		}
	}

	return os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o664)
}
