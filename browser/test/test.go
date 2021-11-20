package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func main() {
	tun, tnet, err := netstack.CreateNetTUN(
		[]net.IP{net.ParseIP("10.100.0.2")},
		[]net.IP{net.ParseIP("8.8.8.8")},
		1420)
	if err != nil {
		log.Panic(err)
	}

	clientKey, _ := wgtypes.ParseKey("WI+uoQD9jGi+nyifmFwmswQu5r0uWFH31WeSmfU0snI=")
	publicServerkey, _ := wgtypes.ParseKey("Xp2HRQ1AJ1WbSrHV1NNHAIcmirLUjUh9jz3K3n4OcgQ=")
	fmt.Printf(clientKey.PublicKey().String())

	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelVerbose, ""))

	err = dev.IpcSet(fmt.Sprintf("private_key=%s\npublic_key=%s\npersistent_keepalive_interval=1\nendpoint=65.21.255.241:51820\nallowed_ip=0.0.0.0/0",
		hex.EncodeToString(clientKey[:]),
		hex.EncodeToString(publicServerkey[:]),
	))
	if err != nil {
		log.Panic(err)
	}
	err = dev.Up()
	if err != nil {
		log.Panic(err)
	}

	client := http.Client{
		Transport: &http.Transport{
			DialContext: tnet.DialContext,
		},
	}
	resp, err := client.Get("https://httpbin.org/ip")
	if err != nil {
		log.Panic(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Panic(err)
	}
	log.Println(string(body))
	time.Sleep(30 * time.Second)
}