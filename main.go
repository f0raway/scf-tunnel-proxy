package main

import (
	"flag"
	"log"
)

func main() {
	node := flag.Bool("node", false, "run SCF WebSocket node")
	config := flag.String("config", "/etc/scf-tunnel-proxy/agent.json", "agent config")
	flag.Parse()
	var err error
	if *node {
		err = runNode()
	} else {
		err = runAgent(*config)
	}
	if err != nil {
		log.Fatal(err)
	}
}
