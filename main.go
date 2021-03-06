package main

import (
	"flag"
	"net"
	"os"
	"time"

	"os/signal"

	"syscall"

	"sync"

	"github.com/webkom/KAFFE/observers"

	"net/http"

	"github.com/kidoman/embd"
	"github.com/kidoman/embd/convertors/mcp3008"
	_ "github.com/kidoman/embd/host/rpi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

func main() {
	var hubot = flag.String("hubot", "https://hubot.abakus.no/moccamaster", "hubot url")
	var hubotToken = flag.String("hubotToken", "", "hubot token")
	var slackToken = flag.String("slacktoken", "", "slack bot token")
	var slackChannel = flag.String("slackchannel", "#general", "slack channel")
	flag.Parse()

	if *hubot == "" || *hubotToken == "" {
		log.Fatalf("The hubot and hubotToken flag cannot be empty")
	}

	if *slackToken != "" && *slackChannel != "" {
		go func() {
			log.Info("watching external ip")
			var lastIP net.IP
			for {
				time.Sleep(60 * time.Second)
				ip, err := GetOutboundIP()
				if err != nil {
					log.Warn("Could not find outbound ip: %v", err)
					continue
				}
				if !lastIP.Equal(ip) {
					err = PostToSlack(*slackToken, *slackChannel, ip)
					if err != nil {
						log.Warn("Could not post message to slack: %v", err)
					}
					lastIP = ip
				}
			}
		}()
	}

	if err := embd.InitGPIO(); err != nil {
		panic(err)
	}
	defer embd.CloseGPIO()

	if err := embd.InitSPI(); err != nil {
		panic(err)
	}
	defer embd.CloseSPI()

	const (
		channel = 0
		speed   = 1000000
		bpw     = 8
		delay   = 0
	)

	spiBus := embd.NewSPIBus(embd.SPIMode0, channel, speed, bpw, delay)
	defer spiBus.Close()
	adc := mcp3008.New(mcp3008.SingleMode, spiBus)

	var mutex = &sync.Mutex{}
	var failure = make(chan error, 1)

	metrics := []MetricObserver{
		//observers.NewPlateModeObserver(adc, mutex),
		//observers.NewPowerObserver(adc, mutex),
		observers.NewWaterContainerObserver(adc, mutex, *hubot, *hubotToken),
		//observers.NewPlateTempObserver(adc, mutex),
		//observers.NewWaterFlowObserver(),
	}

	registry := prometheus.NewRegistry()
	for _, observer := range metrics {
		log.Infof("Adding and starting observer: %v", observer)
		registry.MustRegister(observer.Collector())
	}

	for _, observer := range metrics {
		// Run the observer as a background goroutine
		go func(ob MetricObserver) {
			log.Infof("Starting observer: %v", observer)
			err := ob.Run()
			if err != nil {
				failure <- err
			}
			log.Infof("Observer worker stopped: %v", observer)
		}(observer)

		// Run the observer function in a loop with 10s delay
		go func(ob MetricObserver) {
			for {
				err := ob.Observe()
				if err != nil {
					failure <- err
				}
				time.Sleep(10 * time.Second)
			}
		}(observer)
	}

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, os.Interrupt, syscall.SIGTERM)

	go func() {
		addr := ":8081"
		http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		log.Infof("Prometheus handler is listening on %v", addr)
		err := http.ListenAndServe(addr, nil)
		if err != nil {
			failure <- err
		}
	}()

	select {
	case sig := <-terminate:
		log.Errorf("Received signal: %v", sig)
	case err := <-failure:
		log.Errorf("Internal error: %v", err)
	}

	for _, observer := range metrics {
		err := observer.Stop()
		if err != nil {
			log.Errorf("Could not stop observer %v: %v", observer, err)
		}
	}
}
