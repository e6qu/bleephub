// Command s3fake serves the counting in-process object store on a fixed
// address, so that any S3-speaking tool can be pointed at it by hand, and
// prints what the tool cost when interrupted.
//
//	go run ./s3fake/cmd/s3fake -addr 127.0.0.1:9000 -latency 5ms
//
// Any bucket exists, any credentials sign, and nothing is kept on disk.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/gitstore/s3fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "address to listen on")
	latency := flag.Duration("latency", 0, "delay added to every request")
	trace := flag.Bool("trace", false, "log every request as it is served")
	flag.Parse()

	server, err := s3fake.Listen(*addr)
	if err != nil {
		log.Fatalf("s3fake: %v", err)
	}
	defer server.Close()
	server.SetLatency(*latency)
	if *trace {
		server.SetTrace(func(r *http.Request) {
			line := r.Method + " " + r.URL.Path
			if r.URL.RawQuery != "" {
				line += "?" + r.URL.RawQuery
			}
			if extent := r.Header.Get("Range"); extent != "" {
				line += " [" + extent + "]"
			}
			for _, condition := range []string{"If-Match", "If-None-Match"} {
				if value := r.Header.Get(condition); value != "" {
					line += " [" + condition + ": " + value + "]"
				}
			}
			// Quoted: the line is made of what a client sent.
			log.Print(strconv.Quote(line))
		})
	}
	fmt.Fprintf(os.Stderr, "s3fake listening on %s (path-style, any bucket, any credentials)\n", server.URL())

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	started := time.Now()
	<-interrupt
	fmt.Fprintf(os.Stderr, "\n%s over %s\n", server.Snapshot(), time.Since(started).Round(time.Millisecond))
}
