// Command ghfake serves the GitHub fake on a local port, for a harness that
// drives it out of process (its admin API, under /_fake/, builds the world
// and reads it back). A test harness only; it is never GitHub.
//
//	ghfake -listen 127.0.0.1:0 -url-file /tmp/ghfake.url
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/NodeSpy/conductor-plugins/internal/githubkit/ghfake"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to serve on")
	urlFile := flag.String("url-file", "", "write the fake's base URL here once it is serving")
	flag.Parse()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	f := ghfake.New(ghfake.Options{})
	u := "http://" + ln.Addr().String()
	f.SetBaseURL(u)
	if *urlFile != "" {
		if err := os.WriteFile(*urlFile, []byte(u+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("ghfake: serving %s", u)
	log.Fatal(http.Serve(ln, f.Handler()))
}
