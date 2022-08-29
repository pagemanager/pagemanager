package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/pagemanager/pagemanager"
)

func main() {
	pm, err := pagemanager.New(&pagemanager.Config{})
	if err != nil {
		log.Fatal(err)
	}
	addr := "localhost:8070"
	fmt.Println("listening on " + addr)
	http.ListenAndServe(addr, pm.Pagemanager(pm.NotFoundHandler()))
}
