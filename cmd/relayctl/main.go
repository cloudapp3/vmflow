package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:19090", "admin api base url")
	token := flag.String("token", os.Getenv("VMFLOW_ADMIN_TOKEN"), "admin api bearer token (or VMFLOW_ADMIN_TOKEN)")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	var method string
	var path string
	switch args[0] {
	case "health":
		method = http.MethodGet
		path = "/healthz"
	case "rules":
		method = http.MethodGet
		path = "/v1/rules"
	case "stats":
		method = http.MethodGet
		path = "/v1/stats"
	case "metrics":
		method = http.MethodGet
		path = "/metrics"
	case "precheck":
		method = http.MethodPost
		path = "/v1/precheck"
	case "reload":
		method = http.MethodPost
		path = "/v1/reload"
	default:
		usage()
		os.Exit(1)
	}

	status, body, err := doRequest(*addr, *token, method, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if status >= 400 {
		fmt.Fprint(os.Stderr, string(body))
		os.Exit(1)
	}
	fmt.Print(string(body))
}

func doRequest(baseURL, token, method, path string) (int, []byte, error) {
	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relayctl [-addr http://127.0.0.1:19090] [-token token] <health|rules|stats|metrics|precheck|reload>")
}
