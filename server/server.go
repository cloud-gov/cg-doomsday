package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/gorilla/mux"
	"github.com/thomasmmitchell/doomsday"
	"github.com/thomasmmitchell/doomsday/server/auth"
	"github.com/thomasmmitchell/doomsday/storage"
)

type server struct {
	Core *doomsday.Core
}

func Start(conf Config) error {
	var err error

	logWriter := os.Stderr
	if conf.Server.LogFile != "" {
		logWriter, err = os.OpenFile(conf.Server.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("Could not open log file for writing: %s", err)
		}
	}

	fmt.Fprintf(logWriter, "Initializing server\n")
	fmt.Fprintf(logWriter, "Configuring targeted storage backends\n")

	backends := make([]storage.Accessor, 0, len(conf.Backends))
	for _, b := range conf.Backends {
		fmt.Fprintf(logWriter, "Configuring backend `%s' of type `%s'\n", b.Name, b.Type)
		thisBackend, err := storage.NewAccessor(&b)
		if err != nil {
			return fmt.Errorf("Error configuring backend `%s': %s", b.Name, err)
		}

		backends = append(backends, thisBackend)
	}

	fmt.Fprintf(logWriter, "Setting up doomsday core components\n")

	core := &doomsday.Core{
		Backends: backends,
	}

	core.SetCache(doomsday.NewCache())

	populate := func() {
		startedAt := time.Now()
		err := core.Populate()
		if err != nil {
			fmt.Fprintf(logWriter, "%s: Error populating cache: %s\n", time.Now(), err)
		}
		fmt.Printf("Populate took %s\n", time.Since(startedAt))
	}

	go func() {
		populate()
		interval := time.NewTicker(time.Hour)
		defer interval.Stop()
		for range interval.C {
			populate()
		}
	}()

	fmt.Fprintf(logWriter, "Began asynchronous cache population\n")

	fmt.Fprintf(logWriter, "Configuring frontend authentication\n")
	authorizer, err := auth.NewAuth(conf.Server.Auth)
	if err != nil {
		return err
	}

	auth := authorizer.TokenHandler()
	router := mux.NewRouter()
	router.HandleFunc("/v1/info", getInfo(authorizer.Identifier())).Methods("GET")
	router.HandleFunc("/v1/auth", authorizer.LoginHandler()).Methods("POST")
	router.HandleFunc("/v1/cache", auth(getCache(core))).Methods("GET")
	router.HandleFunc("/v1/cache/refresh", auth(refreshCache(core))).Methods("POST")

	fmt.Fprintf(logWriter, "Beginning listening on port %d\n", conf.Server.Port)

	if conf.Server.TLS.Cert != "" || conf.Server.TLS.Key != "" {
		err = listenAndServeTLS(&conf, router)
	} else {
		err = http.ListenAndServe(fmt.Sprintf(":%d", conf.Server.Port), router)
	}

	return err
}

func listenAndServeTLS(conf *Config, handler http.Handler) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", conf.Server.Port))
	if err != nil {
		return err
	}

	defer ln.Close()

	cert, err := tls.X509KeyPair([]byte(conf.Server.TLS.Cert), []byte(conf.Server.TLS.Key))
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(ln, &tls.Config{
		NextProtos:   []string{"http/1.1"},
		Certificates: []tls.Certificate{cert},
	})

	return http.Serve(tlsListener, handler)
}

func getInfo(authType auth.AuthType) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(struct {
			Version  string `json:"version"`
			AuthType string `json:"auth_type"`
		}{
			Version:  doomsday.Version,
			AuthType: string(authType),
		})
		if err != nil {
			panic("Could not marshal info into json")
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}
}

func getCache(core *doomsday.Core) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {

		data := core.Cache().Map()
		items := make([]doomsday.CacheItem, 0, len(data))
		for k, v := range data {
			items = append(items, doomsday.CacheItem{
				BackendName: v.Backend,
				Path:        k,
				CommonName:  v.Subject.CommonName,
				NotAfter:    v.NotAfter.Unix(),
			})
		}

		sort.Slice(items, func(i, j int) bool { return items[i].NotAfter < items[j].NotAfter })

		resp, err := json.Marshal(&doomsday.GetCacheResponse{Content: items})
		if err != nil {
			w.WriteHeader(500)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write(resp)
		}
	}
}

func refreshCache(core *doomsday.Core) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		go core.Populate()
		w.WriteHeader(204)
	}
}
