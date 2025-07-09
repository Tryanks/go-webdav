package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/emersion/go-webdav"
	"github.com/gofiber/fiber/v2"
)

const (
	MethodMkcol     = "MKCOL"
	MethodCopy      = "COPY"
	MethodMove      = "MOVE"
	MethodLock      = "LOCK"
	MethodUnlock    = "UNLOCK"
	MethodPropfind  = "PROPFIND"
	MethodProppatch = "PROPPATCH"
)

var Methods = []string{
	MethodMkcol,
	MethodCopy, MethodMove,
	MethodLock, MethodUnlock,
	MethodPropfind, MethodProppatch,
}

var ExtendedMethods = append(fiber.DefaultMethods[:], Methods...)

func main() {
	var addr string
	flag.StringVar(&addr, "addr", ":8080", "listening address")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [options...] [directory]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	path := flag.Arg(0)
	if path == "" {
		path = "."
	}

	handler := webdav.Handler{
		FileSystem: webdav.LocalFileSystem(path),
		LockSystem: webdav.NewMemoryLockSystem(),
	}

	app := fiber.New(fiber.Config{
		DisableDefaultDate:       true,
		DisableHeaderNormalizing: true,
		RequestMethods:           ExtendedMethods,
	})
	app.Use("/*", func(c *fiber.Ctx) error {
		return handler.Handle(c)
	})

	log.Printf("WebDAV server listening on %v", addr)
	log.Fatal(app.Listen(addr))
}
