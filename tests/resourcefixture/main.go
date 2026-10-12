// A bounded integration-test workload for CPU throttling and cgroup OOM tests.
package main

import (
	"crypto/sha256"
	"net/http"
	"os"
	"runtime"
	"sync"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "burn" {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				data := []byte("resource-cpu-fixture")
				for {
					sum := sha256.Sum256(data)
					data = sum[:]
				}
			}()
		}
		wg.Wait()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "allocate" {
		var held [][]byte
		for i := 0; i < 512; i++ {
			b := make([]byte, 1<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			held = append(held, b)
		}
		runtime.KeepAlive(held)
		select {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	if err := http.ListenAndServe("0.0.0.0:3000", mux); err != nil {
		panic(err)
	}
}
