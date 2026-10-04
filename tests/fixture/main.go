// The smoke fixture exercises the real Līdza runtime inside release containers.
package main

import (
	"github.com/agim/lidza"
	"net/http"
	"os"
)

func main() {
	lidza.Run(lidza.App{Name: "deployment-smoke", Frontend: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(os.Getenv("RELEASE_TEXT"))) })})
}
