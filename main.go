package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/koron/go-ssdp"
	"github.com/lucsky/go-exml"
)

var target *string
var verb *bool
var transcode *bool

var ad *ssdp.Advertiser
var listen string
var ipPortRegex *regexp.Regexp

type Iface struct {
	InterfaceName string
	InterfaceIP   string
}

func onSearch(m *ssdp.SearchMessage) {
	if strings.Contains(m.Type, "ssdp:all") || strings.Contains(m.Type, "service:ContentDirectory") || strings.Contains(m.Type, "service:ConnectionManager") || strings.Contains(m.Type, "device:MediaServer") {
		ad.Alive()
		if *verb {
			log.Printf("Search: From=%s Type=%s\n", m.From.String(), m.Type)
		}
	} else {
		if *verb {
			log.Printf("Search Unknown: From=%s Type=%s\n", m.From.String(), m.Type)
		}
	}
}

func rewriteBody(resp *http.Response) (err error) {
	for _, val := range resp.Header["Content-Type"] {
		if strings.Contains(val, "xml") {
			if *verb {
				log.Println("Rewrote XML response")
			}

			b, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("read error")
				return err
			}
			err = resp.Body.Close()
			if err != nil {
				return err
			}

			//b = bytes.Replace(b, []byte("http://"+*target+"/"), []byte("http://"+listen+"/"), -1) // replace original url with proxy url
			b = ipPortRegex.ReplaceAll(b, []byte(listen))
			body := io.NopCloser(bytes.NewReader(b))
			resp.Body = body
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		}
		if strings.Contains(val, "audio/ogg") && *transcode {
			log.Println("OGG audio will be transcoded to FLAC")
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			err = resp.Body.Close()
			if err != nil {
				return err
			}

			cmd := exec.Command("ffmpeg", "-y", // Yes to all
				"-i", "pipe:0", // take stdin as input
				"-c:a", "flac", // use mp3 lame codec
				"-f", "flac", // using mp3 muxer (IMPORTANT, output data to pipe require manual muxer selecting)
				"-map_metadata", "0",
				"-sample_fmt", "s16",
				"pipe:1", // output to stdout
			)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout        // stdout result will be written here
			stdin, _ := cmd.StdinPipe() // Open stdin pipe
			cmd.Start()                 // Start a process on another goroutine
			stdin.Write(b)              // pump audio data to stdin pipe
			stdin.Close()               // close the stdin, or ffmpeg will wait forever
			cmd.Wait()                  // wait until ffmpeg finish

			body := io.NopCloser(bytes.NewReader(stdout.Bytes()))
			resp.Body = body
			resp.ContentLength = int64(len(stdout.Bytes()))
			resp.Header.Set("Content-Length", strconv.Itoa(len(stdout.Bytes())))
			resp.Header.Set("Content-Type", "audio/flac")
			return nil
		}
		/*
			if strings.Contains(val, "video") && *transcode {
				log.Println("transcoded")
				orig := resp.Body

				cmd := exec.Command("ffmpeg", "-y", // Yes to all
					"-f", "mp4",
					"-probesize", "8192000",
					"-blocksize", "8192000",
					"-i", "pipe:0", // take stdin as input
					"-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "high", "-level", "5.0",
					"-movflags", "+faststart+frag_keyframe+empty_moov",
					"-f", "mp4",
					"pipe:1", // output to stdout
				)
				pipeReader, _ := cmd.StdoutPipe() // stdout result will be written here
				//cmd.Stderr =
				stdin, _ := cmd.StdinPipe() // Open stdin pipe
				cmd.Start()                 // Start a process on another goroutine
				go func() {
					io.Copy(stdin, orig)
					stdin.Close()     // close the stdin, or ffmpeg will wait forever
					err := cmd.Wait() //cmd.Wait()// wait until ffmpeg finish
					if err != nil {
						log.Printf("command %s failed: %s", cmd, err)
					}
				}()

				resp.Body = pipeReader
				resp.Header.Del("Content-Length")
				resp.ContentLength = int64(-1)
				resp.Header.Set("transferMode.dlna.org", "streaming")
				resp.Header.Set("Content-Type", "video/mpeg")
				return nil
			}*/

	}
	return nil
}

func main() {
	ipPortRegex = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\:\d+`)

	ifname := flag.String("ifname", "", "listen interface name")
	bind := flag.String("bind", "", "bind address")
	bindport := flag.Int("port", 0, "bind port")
	target = flag.String("target", "", "The IP and port of the target")
	pidfile := flag.String("pidfile", "", "The pidfile")
	transcode = flag.Bool("transcode", false, "Transcode unsupported audio (experimental)")
	maxAge := flag.Int("maxage", 1800, "cache control, max-age")
	ai := flag.Int("ai", 10, "alive interval")
	verb = flag.Bool("v", true, "verbose mode")
	h := flag.Bool("h", false, "show help")

	suuid := flag.String("uuid", "", "device uuid")
	sfriendlyName := flag.String("friendlyName", "", "friendlyName")
	sdeviceType := flag.String("deviceType", "", "deviceType")

	flag.Parse()

	if *h {
		flag.PrintDefaults()
		return
	}
	if *target == "" {
		fmt.Println("No target specified!")
		flag.PrintDefaults()
		return
	}

	var err error
	interfaces, err := net.Interfaces()
	if err != nil {
		panic(err)
	}

	ifaceMap := make(map[int]Iface)
	idx := 0
	for _, iface := range interfaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if !addr.(*net.IPNet).IP.IsLoopback() && addr.(*net.IPNet).IP.IsPrivate() {
				i := Iface{
					InterfaceName: iface.Name,
					InterfaceIP:   addr.(*net.IPNet).IP.String(),
				}
				ifaceMap[idx] = i
				idx++
			}
		}
	}
	if idx == 0 {
		panic("network not available")
	}
	if *verb {
		log.Println("available if & ip")
		for c := 0; c < idx; c++ {
			log.Printf("%d: %s (%s)", c+1, ifaceMap[c].InterfaceName, ifaceMap[c].InterfaceIP)
		}
	}
	if *ifname == "" || *bind == "" {
		for c := 0; c < idx; c++ {
			if *ifname == "" && ifaceMap[c].InterfaceIP == *bind {
				ifnamev := ifaceMap[c].InterfaceName
				ifname = &ifnamev
				break
			}
			if *ifname == "" && *bind == "" {
				ifnamev := ifaceMap[c].InterfaceName
				ifname = &ifnamev
				bindv := ifaceMap[c].InterfaceIP
				bind = &bindv
				break
			}
			if ifaceMap[c].InterfaceName == *ifname && *bind == "" {
				bindv := ifaceMap[c].InterfaceIP
				bind = &bindv
				break
			}
		}
	}

	if *ifname == "" || *bind == "" {
		log.Print("Select interface and(or) bind address ")
		flag.PrintDefaults()
		return
	}

	listen = *bind + ":" + strconv.Itoa(*bindport)

	urladdr := "http://" + *target + "/"
	remote, err := url.Parse(urladdr)
	if err != nil {
		panic(err)
	}

	handler := func(p *httputil.ReverseProxy) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			if *verb {
				log.Println(r.RemoteAddr + " requests " + r.URL.RequestURI())
			}
			r.Host = remote.Host
			p.ServeHTTP(w, r)
		}
	}

	listener, _ := net.Listen("tcp", listen)
	listen = listener.Addr().String()
	log.Println("Listening on " + listen)

	go func() {
		proxy := httputil.NewSingleHostReverseProxy(remote)
		proxy.ModifyResponse = rewriteBody

		http.HandleFunc("/", handler(proxy))
		http.Serve(listener, nil)
	}()

	if *pidfile != "" {
		f, _ := os.Create(*pidfile)
		f.WriteString(fmt.Sprint(os.Getpid()))
		f.Close()
		defer os.Remove(*pidfile)
	}

	uuidv := *suuid
	friendlyName := *sfriendlyName
	deviceType := *sdeviceType

	for uuidv == "" {
		resp, err := http.Get(urladdr + "rootDesc.xml")
		if err == nil {
			reader := resp.Body
			decoder := exml.NewDecoder(reader)
			decoder.OnTextOf("root/device/deviceType", exml.Assign(&deviceType))
			decoder.OnTextOf("root/device/UDN", exml.Assign(&uuidv))
			decoder.OnTextOf("root/device/friendlyName", exml.Assign(&friendlyName))
			decoder.Run()
			resp.Body.Close()
			if uuidv != "" {
				break
			}
		}
		log.Printf("Failed to get rootDesc, sleep 1 min")
		time.Sleep(time.Minute)
	}
	log.Printf("DdeviceType %s, uuid %s, friendlyName %s\n", deviceType, uuidv, friendlyName)

	listenif, err := net.InterfaceByName(*ifname)
	if err != nil {
		panic(err)
	}
	ssdp.Interfaces = []net.Interface{*listenif}
	ad, err = ssdp.Advertise(
		deviceType, // send as "ST"
		uuidv,      // send as "USN"
		fmt.Sprintf("http://%s/rootDesc.xml", listen), // send as "LOCATION"
		friendlyName, // send as "SERVER"
		*maxAge)      // send as "maxAge" in "CACHE-CONTROL"
	if err != nil {
		panic(err)
	}
	m := &ssdp.Monitor{
		Search: onSearch,
	}
	m.Start()
	aliveTick := time.Tick(time.Second * time.Duration((*ai)))
	// to detect CTRL-C is pressed.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
loop:
	for {
		select {
		case <-aliveTick:
			ad.Alive()
		case <-quit:
			break loop
		}
	}
	if *verb {
		log.Println("bye")
	}
	ad.Bye()
	ad.Close()
}
