package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

const defaultAppID = "6287487"

func main() {
	appID := flag.String("app-id", defaultAppID, "VK application ID")
	flag.Parse()

	oauthURL := fmt.Sprintf(
		"https://oauth.vk.com/authorize?client_id=%s&display=page&redirect_uri=https://oauth.vk.com/blank.html&scope=offline&response_type=token&v=5.274",
		*appID,
	)

	fmt.Println("Opening VK OAuth page in your browser...")
	fmt.Println()
	fmt.Println("After authorizing, copy the access_token from the URL:")
	fmt.Println("  https://oauth.vk.com/blank.html#access_token=vk1.a.XXXXX&...")
	fmt.Println()
	fmt.Println("URL:", oauthURL)
	fmt.Println()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", oauthURL)
	case "linux":
		cmd = exec.Command("xdg-open", oauthURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", oauthURL)
	}
	if cmd != nil {
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
			fmt.Println("Please open the URL above manually.")
		}
	}
}
