package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "vpn-test-ok")
	})
	fmt.Println("target server listening on :80")
	if err := http.ListenAndServe(":80", nil); err != nil {
		fmt.Printf("target server error: %v\n", err)
	}
}
