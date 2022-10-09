package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/pagemanager/pagemanager"
)

func main() {
	flag.Parse()
	pm, err := pagemanager.New(&pagemanager.Config{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("listening on localhost:8070")
	http.ListenAndServe("localhost:8070", pm.Pagemanager(pm.NotFoundHandler()))
}
