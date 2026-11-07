package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/vbocan/tesla-charge-control/deye"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	port := flag.String("port", "8080", "HTTP server port")
	demo := flag.Bool("demo", false, "run against a simulated inverter, wall connector and car (no hardware, no config file needed)")
	flag.Parse()

	var s *server
	if *demo {
		s = newDemoServer()
	} else {
		cfg, err := deye.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("Config error: %v", err)
		}
		s = newServer(cfg)
	}
	go s.pollLoop()

	log.Printf("Dashboard at http://localhost:%s", *port)
	log.Fatal(http.ListenAndServe(":"+*port, s.routes()))
}
