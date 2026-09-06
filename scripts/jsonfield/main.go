// Command jsonfield pulls one value out of the panel's --json output, so the
// Makefile does not have to parse JSON with sed.
//
//	mp token create --json     | go run ./scripts/jsonfield token
//	mp token list --json       | go run ./scripts/jsonfield last-id
//	mp config show --json      | go run ./scripts/jsonfield panel-url
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: jsonfield token|last-id|panel-url")
		os.Exit(2)
	}
	var doc any
	if err := json.NewDecoder(os.Stdin).Decode(&doc); err != nil {
		fmt.Fprintln(os.Stderr, "jsonfield:", err)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "token":
		fmt.Println(str(dig(doc, "token")))
	case "last-id":
		list, _ := doc.([]any)
		if len(list) == 0 {
			fmt.Fprintln(os.Stderr, "jsonfield: empty list")
			os.Exit(1)
		}
		fmt.Printf("%.0f\n", num(dig(list[len(list)-1], "id")))
	case "panel-url":
		// `mp config show --json` marshals the Go struct, so the keys are
		// capitalised; accept both spellings.
		host := first(str(dig(doc, "Web", "Hostname")), str(dig(doc, "web", "hostname")))
		listen := first(str(dig(doc, "Web", "Listen")), str(dig(doc, "web", "listen")))
		if host == "" {
			host = "localhost"
		}
		port := strings.TrimPrefix(listen, ":")
		if i := strings.LastIndex(listen, ":"); i >= 0 {
			port = listen[i+1:]
		}
		fmt.Printf("https://%s:%s\n", host, port)
	default:
		fmt.Fprintln(os.Stderr, "unknown field", os.Args[1])
		os.Exit(2)
	}
}

// dig walks nested JSON objects.
func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// first returns the first non-empty string.
func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func str(v any) string  { s, _ := v.(string); return s }
func num(v any) float64 { f, _ := v.(float64); return f }
