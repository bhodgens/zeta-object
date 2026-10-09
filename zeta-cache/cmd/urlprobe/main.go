package main

// urlprobe — a one-off diagnostic (zfs-validate ztags session): prints the
// exact URL the zeta-cache transport builds for the ?events key so the
// wire form can be compared against the gateway's routing. NOT shipped.
import (
	"fmt"
	"net/url"
	"strings"
)

func resourceURL(base, key string) string {
	p := "/" + "zval" + "/"
	if key != "" {
		p += strings.TrimPrefix(key, "/")
		if !strings.HasSuffix(p, "/") && strings.HasSuffix(key, "/") {
			p += "/"
		}
	}
	return base + (&url.URL{Path: p}).EscapedPath()
}

func main() {
	fmt.Printf("%q\n", resourceURL("https://127.0.0.1:9713", "?events&since-id=0"))
}
