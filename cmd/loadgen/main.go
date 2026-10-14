// Команда loadgen — простой генератор нагрузки для проверки редиректа.
//
// Пример:
//
//	go run ./cmd/loadgen -url http://localhost:8080/abc1234 -c 64 -d 10s
//
// Редиректы не отслеживаются: измеряется именно ответ сервиса (302).
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	lat    []time.Duration
	status map[int]int
	errs   int
}

func main() {
	var (
		target = flag.String("url", "http://localhost:8080/", "URL для запросов (GET)")
		conc   = flag.Int("c", 16, "число параллельных соединений (воркеров)")
		dur    = flag.Duration("d", 10*time.Second, "длительность теста (игнорируется, если задан -n)")
		total  = flag.Int("n", 0, "общее число запросов (0 — работать по времени -d)")
	)
	flag.Parse()
	if *conc < 1 {
		fmt.Fprintln(os.Stderr, "-c должно быть >= 1")
		os.Exit(2)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *conc,
			MaxIdleConnsPerHost: *conc,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var (
		issued   atomic.Int64 // сколько запросов уже выдано воркерам (для -n)
		deadline time.Time
	)
	if *total <= 0 {
		deadline = time.Now().Add(*dur)
	}

	results := make([]result, *conc)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func(r *result) {
			defer wg.Done()
			r.status = map[int]int{}
			for {
				if *total > 0 {
					if issued.Add(1) > int64(*total) {
						return
					}
				} else if !time.Now().Before(deadline) {
					return
				}
				t0 := time.Now()
				resp, err := client.Get(*target)
				if err != nil {
					r.errs++
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				r.lat = append(r.lat, time.Since(t0))
				r.status[resp.StatusCode]++
			}
		}(&results[w])
	}
	wg.Wait()
	elapsed := time.Since(start)

	var lat []time.Duration
	status := map[int]int{}
	errs := 0
	for _, r := range results {
		lat = append(lat, r.lat...)
		errs += r.errs
		for k, v := range r.status {
			status[k] += v
		}
	}
	report(*target, *conc, elapsed, lat, status, errs)
}

func report(target string, conc int, elapsed time.Duration, lat []time.Duration, status map[int]int, errs int) {
	ok := len(lat)
	fmt.Printf("url:          %s\n", target)
	fmt.Printf("concurrency:  %d\n", conc)
	fmt.Printf("duration:     %.2fs\n", elapsed.Seconds())
	fmt.Printf("requests:     %d (с ответом), %d (ошибки транспорта)\n", ok, errs)
	fmt.Printf("RPS:          %.1f\n", float64(ok)/elapsed.Seconds())

	codes := make([]int, 0, len(status))
	for c := range status {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	fmt.Print("status:      ")
	for _, c := range codes {
		fmt.Printf(" %d=%d", c, status[c])
	}
	fmt.Println()

	if ok == 0 {
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		idx := int(float64(len(lat))*p+0.5) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(lat) {
			idx = len(lat) - 1
		}
		return lat[idx]
	}
	fmt.Printf("latency:      p50=%s p90=%s p95=%s p99=%s max=%s\n",
		pct(0.50), pct(0.90), pct(0.95), pct(0.99), lat[len(lat)-1])
}
