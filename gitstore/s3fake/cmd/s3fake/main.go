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
	"strings"
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
			// Quoted as well: whatever else a client put in its path or prefix
			// stays inside the one line.
			log.Print(strconv.Quote(traceLine(r)))
		})
	}
	fmt.Fprintf(os.Stderr, "s3fake listening on %s (path-style, any bucket, any credentials)\n", server.URL())

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	started := time.Now()
	<-interrupt
	fmt.Fprintf(os.Stderr, "\n%s over %s\n", server.Snapshot(), time.Since(started).Round(time.Millisecond))
}

// traceLine describes a request by what tells one from another: the method and
// key, a listing's prefix and delimiter, a ranged read's extent, and whether the
// write was conditional. It prints no header value and no other query
// parameter — a presigned URL carries its credential and signature there, and a
// trace is something people paste into bug reports.
func traceLine(r *http.Request) string {
	line := printable(r.Method) + " " + printable(r.URL.Path)
	query := r.URL.Query()
	for _, name := range []string{"prefix", "delimiter"} {
		if query.Has(name) {
			line += " " + name + "=" + printable(query.Get(name))
		}
	}
	for _, name := range []string{"uploads", "uploadId", "partNumber", "delete"} {
		if query.Has(name) {
			line += " " + name
		}
	}
	var first, last int64
	if n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last); n == 2 {
		line += fmt.Sprintf(" [bytes %d-%d]", first, last)
	}
	switch {
	case r.Header.Get("If-None-Match") == "*":
		line += " [if absent]"
	case r.Header.Get("If-None-Match") != "":
		line += " [if changed]"
	case r.Header.Get("If-Match") != "":
		line += " [if unchanged]"
	}
	return line
}

// printable keeps a client-supplied string from starting a log line of its own.
func printable(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
}
