package main


import (
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "io/ioutil"
    "log"
    "math/rand"
    "net/http"
    "os"
    "os/signal"
    "runtime"
    "strconv"
    "syscall"
    "time"
)


const (
    // defaultTerminationDelay delays termination of the program in a graceful shutdown situation.
    // We do this to prevent the pod from exiting immediately upon a pod termination event
    // (e.g. during a rolling update). This gives some time for ingress controllers to react to
    // the Pod IP being removed from the Service's Endpoint list, which prevents traffic from being
    // directed to terminated pods, which otherwise would cause timeout errors and/or request delays.
    // See: https://github.com/kubernetes/ingress-nginx/issues/3335#issuecomment-434970950
    defaultTerminationDelay = 10
)


var (
    color  = os.Getenv("COLOR")
    colors = []string{
       
        "green",
       
    }
    envLatency   float64
    envErrorRate int
    dynamicLatency float64 // New variable to handle dynamic latency increment

)


func init() {
    var err error
    envLatencyStr := os.Getenv("LATENCY")
    if envLatencyStr != "" {
        envLatency, err = strconv.ParseFloat(envLatencyStr, 64)
        if err != nil {
            panic(fmt.Sprintf("failed to parse LATENCY: %s", envLatencyStr))
        }
    }
    dynamicLatency = 0 // Initialize dynamic latency
}


func main() {
    var (
        listenAddr       string
        terminationDelay int
        numCPUBurn       string
        tls              bool
    )
    flag.StringVar(&listenAddr, "listen-addr", ":8080", "server listen address")
    flag.IntVar(&terminationDelay, "termination-delay", defaultTerminationDelay, "termination delay in seconds")
    flag.StringVar(&numCPUBurn, "cpu-burn", "", "burn specified number of cpus (number or 'all')")
    flag.BoolVar(&tls, "tls", false, "Enable TLS (with self-signed certificate)")
    flag.Parse()

    rand.Seed(time.Now().UnixNano())

    router := http.NewServeMux()
    router.Handle("/", http.StripPrefix("/", http.FileServer(http.Dir("./"))))
    router.HandleFunc("/color", getColor)

    server := &http.Server{
        Addr:    listenAddr,
        Handler: router,
    }
    if tls {
        tlsConfig, err := CreateServerTLSConfig("", "", []string{"localhost", "rollouts-demo"})
        if err != nil {
            log.Fatalf("Could not generate TLS config: %v\n", err)
        }
        server.TLSConfig = tlsConfig
    }

    done := make(chan bool)
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

    go func() {
        ticker := time.NewTicker(60 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
				if dynamicLatency < 5 {
					dynamicLatency += 1 // Increase latency by 0.2 seconds every 30 seconds
				}
            case <-quit:
                return
            }
        }
    }()

    go func() {
        sig := <-quit
        server.SetKeepAlivesEnabled(false)
        log.Printf("Signal %v caught. Shutting down in %vs", sig, terminationDelay)
        delay := time.NewTicker(time.Duration(terminationDelay) * time.Second)
        defer delay.Stop()
        select {
        case <-quit:
            log.Println("Second signal caught. Shutting down NOW")
        case <-delay.C:
        }

        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        if err := server.Shutdown(ctx); err != nil {
            log.Fatalf("Could not gracefully shutdown the server: %v\n", err)
        }
        close(done)
    }()

    cpuBurn(done, numCPUBurn)
    log.Printf("Started server on %s", listenAddr)
    var err error
    if tls {
        err = server.ListenAndServeTLS("", "")
    } else {
        err = server.ListenAndServe()
    }
    if err != nil && err != http.ErrServerClosed {
        log.Fatalf("Could not listen on %s: %v\n", listenAddr, err)
    }

    <-done
    log.Println("Server stopped")
}


type colorParameters struct {
    Color                string  `json:"color"`
    DelayLength          float64 `json:"delayLength,omitempty"`
    Return500Probability *int    `json:"return500,omitempty"`
}


func getColor(w http.ResponseWriter, r *http.Request) {
    requestBody, err := ioutil.ReadAll(r.Body)
    if err != nil {
        w.WriteHeader(500)
        log.Println(err.Error())
        fmt.Fprintf(w, err.Error())
        return
    }

    var request []colorParameters
    if len(requestBody) > 0 && string(requestBody) != `"[]"` {
        err = json.Unmarshal(requestBody, &request)
        if err != nil {
            w.WriteHeader(500)
            log.Printf("%s: %v", string(requestBody), err.Error())
            fmt.Fprintf(w, err.Error())
            return
        }
    }

    colorToReturn := randomColor()
    if color != "" {
        colorToReturn = color
    }

    var colorParams colorParameters
    for i := range request {
        cp := request[i]
        if cp.Color == colorToReturn {
            colorParams = cp
        }
    }

    var delayLength float64
    if colorParams.DelayLength > 0 {
        delayLength = colorParams.DelayLength
    } else if envLatency > 0 {
        delayLength = envLatency
    }

    // Apply dynamicLatency, incrementing with time
	
	if delayLength > 0{
		delayLength += dynamicLatency
	}

    if delayLength > 0 {
        delayLengthStr := fmt.Sprintf(" (%fs)", delayLength)
        time.Sleep(time.Duration(delayLength) * time.Second)
        log.Printf("%d - %s%s\n", http.StatusOK, colorToReturn, delayLengthStr)
    }

    statusCode := http.StatusOK
    if colorParams.Return500Probability != nil && *colorParams.Return500Probability > 0 && rand.Intn(100) < *colorParams.Return500Probability {
        statusCode = http.StatusInternalServerError
    }

    printColor(colorToReturn, w, statusCode)
}


func printColor(colorToPrint string, w http.ResponseWriter, statusCode int) {
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Header().Set("X-Content-Type-Options", "nosniff")
    w.WriteHeader(statusCode)
    fmt.Fprintf(w, "\"%s\"", colorToPrint)
}


func randomColor() string {
    return colors[rand.Int()%len(colors)]
}


func cpuBurn(done <-chan bool, numCPUBurn string) {
    if numCPUBurn == "" {
        return
    }
    var numCPU int
    if numCPUBurn == "all" {
        numCPU = runtime.NumCPU()
    } else {
        num, err := strconv.Atoi(numCPUBurn)
        if err != nil {
            log.Fatal(err)
        }
        numCPU = num
    }
    log.Printf("Burning %d CPUs", numCPU)
    noop := func() {}
    for i := 0; i < numCPU; i++ {
        go func(cpu int) {
            log.Printf("Burning CPU #%d", cpu)
            for {
                select {
                case <-done:
                    log.Printf("Stopped CPU burn #%d", cpu)
                    return
                default:
                    noop()
                }
            }
        }(i)
    }
}
