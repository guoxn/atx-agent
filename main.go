package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codeskyblue/kexec"
	"github.com/codeskyblue/procfs"
	"github.com/franela/goreq"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/openatx/androidutils"
	"github.com/openatx/atx-agent/cmdctrl"
	"github.com/pkg/errors"
	"github.com/rs/cors"
	"github.com/shogo82148/androidbinary/apk"
)

var (
	service     = cmdctrl.New()
	downManager = newDownloadManager()
	upgrader    = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

// singleFight for http request
// - minicap
// - minitouch
var muxMutex = sync.Mutex{}
var muxLocks = make(map[string]bool)
var muxConns = make(map[string]*websocket.Conn)

func singleFightWrap(handleFunc func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		muxMutex.Lock()
		if _, ok := muxLocks[r.RequestURI]; ok {
			muxMutex.Unlock()
			log.Println("singlefight conflict", r.RequestURI)
			http.Error(w, "singlefight conflicts", http.StatusTooManyRequests) // code: 429
			return
		}
		muxLocks[r.RequestURI] = true
		muxMutex.Unlock()

		handleFunc(w, r) // handle requests

		muxMutex.Lock()
		delete(muxLocks, r.RequestURI)
		muxMutex.Unlock()
	}
}

func singleFightNewerWebsocket(handleFunc func(http.ResponseWriter, *http.Request, *websocket.Conn)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		muxMutex.Lock()
		if oldWs, ok := muxConns[r.RequestURI]; ok {
			oldWs.Close()
			delete(muxConns, r.RequestURI)
		}

		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			http.Error(w, "websocket upgrade error", 500)
			muxMutex.Unlock()
			return
		}
		muxConns[r.RequestURI] = wsConn
		muxMutex.Unlock()

		handleFunc(w, r, wsConn) // handle request

		muxMutex.Lock()
		if muxConns[r.RequestURI] == wsConn { // release connection
			delete(muxConns, r.RequestURI)
		}
		muxMutex.Unlock()
	}
}

// Get preferred outbound ip of this machine
func getOutboundIP() (ip net.IP, err error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP, nil
}

func mustGetOoutboundIP() net.IP {
	ip, err := getOutboundIP()
	if err != nil {
		return net.ParseIP("127.0.0.1")
		// panic(err)
	}
	return ip
}

func GoFunc(f func() error) chan error {
	ch := make(chan error)
	go func() {
		ch <- f()
	}()
	return ch
}

type MinicapInfo struct {
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Rotation int     `json:"rotation"`
	Density  float32 `json:"density"`
}

var (
	propOnce              sync.Once
	properties            map[string]string
	deviceRotation        int
	displayMaxWidthHeight = 800
)

func updateMinicapRotation(rotation int) {
	devInfo := getDeviceInfo()
	width, height := devInfo.Display.Width, devInfo.Display.Height
	service.UpdateArgs("minicap", "/data/local/tmp/minicap", "-S", "-P",
		fmt.Sprintf("%dx%d@%dx%d/%d", width, height, displayMaxWidthHeight, displayMaxWidthHeight, rotation))
}

func getProperty(name string) string {
	propOnce.Do(func() {
		var err error
		properties, err = androidutils.Properties()
		if err != nil {
			log.Println("getProperty err:", err)
			properties = make(map[string]string)
		}
	})
	return properties[name]
}

const (
	apkVersionCode = 4
	apkVersionName = "1.0.4"
)

func checkUiautomatorInstalled() (ok bool) {
	pi, err := androidutils.StatPackage("com.github.uiautomator")
	if err != nil {
		return
	}
	if pi.Version.Code < apkVersionCode {
		return
	}
	_, err = androidutils.StatPackage("com.github.uiautomator.test")
	return err == nil
}

func installAPK(path string) error {
	// -g: grant all runtime permissions
	// -d: allow version code downgrade
	// -r: replace existing application
	sdk, _ := strconv.Atoi(getProperty("ro.build.version.sdk"))
	cmds := []string{"pm", "install", "-d", "-r", path}
	if sdk >= 23 { // android 6.0
		cmds = []string{"pm", "install", "-d", "-r", "-g", path}
	}
	out, err := runShell(cmds...)
	if err != nil {
		matches := regexp.MustCompile(`Failure \[([\w_ ]+)\]`).FindStringSubmatch(string(out))
		if len(matches) > 0 {
			return errors.Wrap(err, matches[0])
		}
		return errors.Wrap(err, string(out))
	}
	return nil
}

var canFixedInstallFails = map[string]bool{
	"INSTALL_FAILED_PERMISSION_MODEL_DOWNGRADE": true,
	"INSTALL_FAILED_UPDATE_INCOMPATIBLE":        true,
	"INSTALL_FAILED_VERSION_DOWNGRADE":          true,
}

func installAPKForce(path string, packageName string) error {
	err := installAPK(path)
	if err == nil {
		return nil
	}
	errType := regexp.MustCompile(`INSTALL_FAILED_[\w_]+`).FindString(err.Error())
	if !canFixedInstallFails[errType] {
		return err
	}
	runShell("pm", "uninstall", packageName)
	return installAPK(path)
}

func Screenshot(filename string) (err error) {
	output, err := runShellOutput("LD_LIBRARY_PATH=/data/local/tmp", "/data/local/tmp/minicap", "-i")
	if err != nil {
		return
	}
	var f MinicapInfo
	if er := json.Unmarshal([]byte(output), &f); er != nil {
		err = fmt.Errorf("minicap not supported: %v", er)
		return
	}
	if _, err = runShell(
		"LD_LIBRARY_PATH=/data/local/tmp",
		"/data/local/tmp/minicap",
		"-P", fmt.Sprintf("%dx%d@%dx%d/%d", f.Width, f.Height, f.Width, f.Height, f.Rotation),
		"-s", ">"+filename); err != nil {
		return
	}
	return nil
}

type DownloadManager struct {
	db map[string]*downloadProxy
	mu sync.Mutex
	n  int
}

func newDownloadManager() *DownloadManager {
	return &DownloadManager{
		db: make(map[string]*downloadProxy, 10),
	}
}

func (m *DownloadManager) Get(id string) *downloadProxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db[id]
}

func (m *DownloadManager) Put(di *downloadProxy) (id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n += 1
	id = strconv.Itoa(m.n)
	m.db[id] = di
	// di.Id = id
	return id
}

func (m *DownloadManager) Del(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.db, id)
}

func (m *DownloadManager) DelayDel(id string, sleep time.Duration) {
	go func() {
		time.Sleep(sleep)
		m.Del(id)
	}()
}

func AsyncDownloadTo(url string, filepath string, autoRelease bool) (di *downloadProxy, err error) {
	// do real http download
	res, err := goreq.Request{
		Uri:             url,
		MaxRedirects:    10,
		RedirectHeaders: true,
	}.Do()
	if err != nil {
		return
	}
	if res.StatusCode != http.StatusOK {
		body, err := res.Body.ToString()
		res.Body.Close()
		if err != nil && err != bytes.ErrTooLarge {
			return nil, fmt.Errorf("Expected HTTP Status code: %d", res.StatusCode)
		}
		return nil, errors.New(body)
	}
	file, err := os.Create(filepath)
	if err != nil {
		res.Body.Close()
		return
	}
	var totalSize int
	fmt.Sscanf(res.Header.Get("Content-Length"), "%d", &totalSize)
	di = newDownloadProxy(file, totalSize)
	downloadKey := downManager.Put(di)
	go func() {
		if autoRelease {
			defer downManager.Del(downloadKey)
		}
		defer di.Done()
		defer res.Body.Close()
		defer file.Close()
		io.Copy(di, res.Body)
	}()
	return
}

func currentUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Name
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	output, err := exec.Command("whoami").Output()
	if err == nil {
		return strings.TrimSpace(string(output))
	}
	return ""
}

func renderHTML(w http.ResponseWriter, filename string) {
	file, err := Assets.Open(filename)
	if err != nil {
		http.Error(w, "404 page not found", 404)
		return
	}
	content, _ := ioutil.ReadAll(file)
	template.Must(template.New(filename).Parse(string(content))).Execute(w, nil)
}

var (
	ErrJpegWrongFormat = errors.New("jpeg format error, not starts with 0xff,0xd8")

	// target, _ := url.Parse("http://127.0.0.1:9008")
	// uiautomatorProxy := httputil.NewSingleHostReverseProxy(target)
	uiautomatorProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "127.0.0.1:9008"
		},
		Transport: &http.Transport{
			// Ref: https://golang.org/pkg/net/http/#RoundTripper
			Dial: func(network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
					DualStack: true,
				}).Dial(network, addr)
				return conn, err
			},
			MaxIdleConns:          100,
			IdleConnTimeout:       180 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
)

type errorBinaryReader struct {
	rd  io.Reader
	err error
}

func (r *errorBinaryReader) ReadInto(datas ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	for _, data := range datas {
		r.err = binary.Read(r.rd, binary.LittleEndian, data)
		if r.err != nil {
			return r.err
		}
	}
	return nil
}

// read from @minicap and send jpeg raw data to channel
func translateMinicap(conn net.Conn, jpgC chan []byte, quitC chan bool) error {
	var pid, rw, rh, vw, vh uint32
	var version, unused, orientation, quirkFlag uint8
	rd := bufio.NewReader(conn)
	binRd := errorBinaryReader{rd: rd}
	err := binRd.ReadInto(&version, &unused, &pid, &rw, &rh, &vw, &vh, &orientation, &quirkFlag)
	if err != nil {
		return err
	}
	for {
		var size uint32
		if err = binRd.ReadInto(&size); err != nil {
			break
		}

		lr := &io.LimitedReader{R: rd, N: int64(size)}
		buf := bytes.NewBuffer(nil)
		_, err = io.Copy(buf, lr)
		if err != nil {
			break
		}
		if string(buf.Bytes()[:2]) != "\xff\xd8" {
			err = ErrJpegWrongFormat
			break
		}
		select {
		case jpgC <- buf.Bytes(): // Maybe should use buffer instead
		case <-quitC:
			return nil
		default:
			// TODO(ssx): image should not wait or it will stuck here
		}
	}
	return err
}

func ServeHTTP(lis net.Listener, tunnel *TunnelProxy) error {
	m := mux.NewRouter()

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		renderHTML(w, "index.html")
	})

	m.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, version)
	})

	m.HandleFunc("/remote", func(w http.ResponseWriter, r *http.Request) {
		renderHTML(w, "remote.html")
	})

	m.HandleFunc("/pidof/{pkgname}", func(w http.ResponseWriter, r *http.Request) {
		pkgname := mux.Vars(r)["pkgname"]
		if pid, err := pidOf(pkgname); err == nil {
			io.WriteString(w, strconv.Itoa(pid))
			return
		}
	})

	m.HandleFunc("/session/{pkgname}", func(w http.ResponseWriter, r *http.Request) {
		packageName := mux.Vars(r)["pkgname"]
		mainActivity, err := mainActivityOf(packageName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusGone) // 410
			return
		}
		flags := r.FormValue("flags")
		if flags == "" {
			flags = "-W -S" // W: wait launched, S: stop before started
		}

		w.Header().Set("Content-Type", "application/json")
		output, err := runShellTimeout(10*time.Second, "am", "start", flags, "-n", packageName+"/"+mainActivity)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":      false,
				"error":        err.Error(),
				"output":       string(output),
				"mainActivity": mainActivity,
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":      true,
				"mainActivity": mainActivity,
				"output":       string(output),
			})
		}
	}).Methods("POST")

	m.HandleFunc("/session/{pid:[0-9]+}:{pkgname}/{url:ping|jsonrpc/0}", func(w http.ResponseWriter, r *http.Request) {
		pkgname := mux.Vars(r)["pkgname"]
		pid, _ := strconv.Atoi(mux.Vars(r)["pid"])

		pfs, err := procfs.NewFS(procfs.DefaultMountPoint)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		proc, err := pfs.NewProc(pid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusGone) // 410
			return
		}
		cmdline, _ := proc.CmdLine()
		if len(cmdline) != 1 || cmdline[0] != pkgname {
			http.Error(w, fmt.Sprintf("cmdline expect [%s] but got %v", pkgname, cmdline), http.StatusGone)
			return
		}
		r.URL.Path = "/" + mux.Vars(r)["url"]
		uiautomatorProxy.ServeHTTP(w, r)
	})

	m.HandleFunc("/shell", func(w http.ResponseWriter, r *http.Request) {
		command := r.FormValue("command")
		if command == "" {
			command = r.FormValue("c")
		}
		c := Command{
			Args:    []string{command},
			Shell:   true,
			Timeout: 1 * time.Minute,
		}
		output, err := c.Output()
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"output": string(output),
			"error":  err,
		})
	}).Methods("GET", "POST")

	m.HandleFunc("/shell/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		command := r.FormValue("command")
		if command == "" {
			command = r.FormValue("c")
		}
		c := exec.Command("sh", "-c", command)

		httpWriter := newFakeWriter(func(data []byte) (int, error) {
			n, err := w.Write(data)
			if err == nil {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			} else {
				log.Println("Write error")
			}
			return n, err
		})
		c.Stdout = httpWriter
		c.Stderr = httpWriter

		// wait until program quit
		cmdQuit := make(chan error, 0)
		go func() {
			cmdQuit <- c.Run()
		}()
		select {
		case <-httpWriter.Err:
			if c.Process != nil {
				c.Process.Signal(syscall.SIGTERM)
			}
		case <-cmdQuit:
			log.Println("command quit")
		}
		log.Println("program quit")
	})

	m.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		log.Println("stop all service")
		service.StopAll()
		log.Println("service stopped")
		io.WriteString(w, "Finished!")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel() // The document says need to call cancel(), but I donot known why.
			httpServer.Shutdown(ctx)
		}()
	})

	m.HandleFunc("/uiautomator", func(w http.ResponseWriter, r *http.Request) {
		err := service.Start("uiautomator")
		if err == nil {
			io.WriteString(w, "Success")
		} else {
			http.Error(w, err.Error(), 500)
		}
	}).Methods("POST")

	m.HandleFunc("/uiautomator", func(w http.ResponseWriter, r *http.Request) {
		err := service.Stop("uiautomator", true) // wait until program quit
		if err == nil {
			io.WriteString(w, "Success")
		} else {
			http.Error(w, err.Error(), 500)
		}
	}).Methods("DELETE")

	m.HandleFunc("/raw/{filepath:.*}", func(w http.ResponseWriter, r *http.Request) {
		filepath := mux.Vars(r)["filepath"]
		http.ServeFile(w, r, filepath)
	})

	m.HandleFunc("/info/battery", func(w http.ResponseWriter, r *http.Request) {
		devInfo := getDeviceInfo()
		devInfo.Battery.Update()
		if err := tunnel.UpdateInfo(devInfo); err != nil {
			// log.Printf("update info err: %v", err)
			io.WriteString(w, "Failure "+err.Error())
			return
		}
		io.WriteString(w, "Success")
	}).Methods("POST")

	m.HandleFunc("/info/rotation", func(w http.ResponseWriter, r *http.Request) {
		var direction int                                 // 0,1,2,3
		err := json.NewDecoder(r.Body).Decode(&direction) // TODO: auto get rotation
		if err == nil {
			deviceRotation = direction * 90
			log.Println("rotation change received:", deviceRotation)
		} else {
			rotation, er := androidutils.Rotation()
			if er != nil {
				log.Println("rotation auto get err:", er)
				http.Error(w, "Failure", 500)
				return
			}
			deviceRotation = rotation
		}
		updateMinicapRotation(deviceRotation)
		fmt.Fprintf(w, "rotation change to %d", deviceRotation)
	})

	m.HandleFunc("/upload/{target:.*}", func(w http.ResponseWriter, r *http.Request) {
		target := mux.Vars(r)["target"]
		if runtime.GOOS != "windows" {
			target = "/" + target
		}
		var fileMode os.FileMode
		if _, err := fmt.Sscanf(r.FormValue("mode"), "%o", &fileMode); err != nil {
			log.Printf("invalid file mode: %s", r.FormValue("mode"))
			fileMode = 0644
		} // %o base 8

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() {
			file.Close()
			r.MultipartForm.RemoveAll()
		}()
		if strings.HasSuffix(target, "/") {
			target = path.Join(target, header.Filename)
		}

		targetDir := filepath.Dir(target)
		if _, err := os.Stat(targetDir); os.IsNotExist(err) {
			os.MkdirAll(targetDir, 0755)
		}

		fd, err := os.Create(target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer fd.Close()
		written, err := io.Copy(fd, file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if fileMode != 0 {
			os.Chmod(target, fileMode)
		}
		if fileInfo, err := os.Stat(target); err == nil {
			fileMode = fileInfo.Mode()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"target": target,
			"size":   written,
			"mode":   fmt.Sprintf("0%o", fileMode),
		})
	})

	m.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		dst := r.FormValue("filepath")
		url := r.FormValue("url")
		var fileMode os.FileMode
		if _, err := fmt.Sscanf(r.FormValue("mode"), "%o", &fileMode); err != nil {
			log.Printf("invalid file mode: %s", r.FormValue("mode"))
			fileMode = 0644
		} // %o base 8
		key := background.HTTPDownload(url, dst, fileMode)
		io.WriteString(w, key)
	}).Methods("POST")

	m.HandleFunc("/download/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := mux.Vars(r)["key"]
		status := background.Get(key)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}).Methods("GET")

	m.HandleFunc("/install", func(w http.ResponseWriter, r *http.Request) {
		var url = r.FormValue("url")
		filepath := TempFileName("/sdcard/tmp", ".apk")
		key := background.HTTPDownload(url, filepath, 0644)
		go func() {
			defer os.Remove(filepath) // release sdcard space

			state := background.Get(key)
			if err := background.Wait(key); err != nil {
				log.Println("http download error")
				state.Error = err.Error()
				state.Message = "http download error"
				return
			}

			state.Message = "apk parsing"
			pkg, er := apk.OpenFile(filepath)
			if er != nil {
				state.Error = er.Error()
				state.Message = "androidbinary parse apk error"
				return
			}
			defer pkg.Close()
			packageName := pkg.PackageName()
			state.PackageName = packageName

			state.Message = "installing"
			if err := installAPKForce(filepath, packageName); err != nil {
				state.Error = err.Error()
				state.Message = "error install"
			} else {
				state.Message = "success installed"
			}
		}()
		io.WriteString(w, key)
	}).Methods("POST")

	m.HandleFunc("/install/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]
		state := background.Get(id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state)
	}).Methods("GET")

	m.HandleFunc("/install/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]
		state := background.Get(id)
		if state.Progress != nil {
			if dproxy, ok := state.Progress.(*downloadProxy); ok {
				dproxy.Cancel()
				io.WriteString(w, "Cancelled")
				return
			}
		}
		io.WriteString(w, "Unable to canceled")
	}).Methods("DELETE")

	m.HandleFunc("/minitouch", singleFightNewerWebsocket(func(w http.ResponseWriter, r *http.Request, ws *websocket.Conn) {
		defer ws.Close()
		const wsWriteWait = 10 * time.Second
		wsWrite := func(messageType int, data []byte) error {
			ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			return ws.WriteMessage(messageType, data)
		}
		wsWrite(websocket.TextMessage, []byte("start @minitouch service"))
		if err := service.Start("minitouch"); err != nil && err != cmdctrl.ErrAlreadyRunning {
			wsWrite(websocket.TextMessage, []byte("@minitouch service start failed: "+err.Error()))
			return
		}
		wsWrite(websocket.TextMessage, []byte("dial unix:@minitouch"))
		log.Printf("minitouch connection: %v", r.RemoteAddr)
		retries := 0
		quitC := make(chan bool, 2)
		operC := make(chan TouchRequest, 10)
		defer func() {
			wsWrite(websocket.TextMessage, []byte("unix:@minitouch websocket closed"))
			close(operC)
		}()
		go func() {
			for {
				if retries > 10 {
					log.Println("unix @minitouch connect failed")
					wsWrite(websocket.TextMessage, []byte("@minitouch listen timeout, possibly minitouch not installed"))
					ws.Close()
					break
				}
				conn, err := net.Dial("unix", "@minitouch")
				if err != nil {
					retries++
					log.Printf("dial @minitouch error: %v, wait 0.5s", err)
					select {
					case <-quitC:
						return
					case <-time.After(500 * time.Millisecond):
					}
					continue
				}
				log.Println("unix @minitouch connected, accepting requests")
				retries = 0 // connected, reset retries
				err = drainTouchRequests(conn, operC)
				conn.Close()
				if err != nil {
					log.Println("drain touch requests err:", err)
				} else {
					log.Println("unix @minitouch disconnected")
					break // operC closed
				}
			}
		}()
		var touchRequest TouchRequest
		for {
			err := ws.ReadJSON(&touchRequest)
			if err != nil {
				log.Println("readJson err:", err)
				quitC <- true
				break
			}
			select {
			case operC <- touchRequest:
			case <-time.After(2 * time.Second):
				wsWrite(websocket.TextMessage, []byte("touch request buffer full"))
			}
		}
	}))

	m.HandleFunc("/minicap", singleFightNewerWebsocket(func(w http.ResponseWriter, r *http.Request, ws *websocket.Conn) {
		defer ws.Close()

		const wsWriteWait = 10 * time.Second
		wsWrite := func(messageType int, data []byte) error {
			ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			return ws.WriteMessage(messageType, data)
		}
		wsWrite(websocket.TextMessage, []byte("restart @minicap service"))
		if err := service.Restart("minicap"); err != nil && err != cmdctrl.ErrAlreadyRunning {
			wsWrite(websocket.TextMessage, []byte("@minicap service start failed: "+err.Error()))
			return
		}

		wsWrite(websocket.TextMessage, []byte("dial unix:@minicap"))
		log.Printf("minicap connection: %v", r.RemoteAddr)
		dataC := make(chan []byte, 10)
		quitC := make(chan bool, 2)

		go func() {
			defer close(dataC)
			retries := 0
			for {
				if retries > 10 {
					log.Println("unix @minicap connect failed")
					dataC <- []byte("@minicap listen timeout, possibly minicap not installed")
					break
				}
				conn, err := net.Dial("unix", "@minicap")
				if err != nil {
					retries++
					log.Printf("dial @minicap err: %v, wait 0.5s", err)
					select {
					case <-quitC:
						return
					case <-time.After(500 * time.Millisecond):
					}
					continue
				}
				dataC <- []byte("rotation " + strconv.Itoa(deviceRotation))
				retries = 0 // connected, reset retries
				if er := translateMinicap(conn, dataC, quitC); er == nil {
					conn.Close()
					log.Println("transfer closed")
					break
				} else {
					conn.Close()
					log.Println("minicap read error, try to read again")
				}
			}
		}()
		go func() {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					quitC <- true
					break
				}
			}
		}()
		for data := range dataC {
			if string(data[:2]) == "\xff\xd8" { // jpeg data
				if err := wsWrite(websocket.BinaryMessage, data); err != nil {
					break
				}
				if err := wsWrite(websocket.TextMessage, []byte("data size: "+strconv.Itoa(len(data)))); err != nil {
					break
				}
			} else {
				if err := wsWrite(websocket.TextMessage, data); err != nil {
					break
				}
			}
		}
		quitC <- true
		log.Println("stream finished")
	}))

	// TODO(ssx): perfer to delete
	// FIXME(ssx): screenrecord is not good enough, need to change later
	var recordCmd *exec.Cmd
	var recordDone = make(chan bool, 1)
	var recordLock sync.Mutex
	var recordFolder = "/sdcard/screenrecords/"
	var recordRunning = false

	m.HandleFunc("/screenrecord", func(w http.ResponseWriter, r *http.Request) {
		recordLock.Lock()
		defer recordLock.Unlock()

		if recordCmd != nil {
			http.Error(w, "screenrecord not closed", 400)
			return
		}
		os.RemoveAll(recordFolder)
		os.MkdirAll(recordFolder, 0755)
		recordCmd = exec.Command("screenrecord", recordFolder+"0.mp4")
		if err := recordCmd.Start(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		recordRunning = true
		go func() {
			for i := 1; recordCmd.Wait() == nil && i <= 20 && recordRunning; i++ { // set limit, to prevent too many videos. max 1 hour
				recordCmd = exec.Command("screenrecord", recordFolder+strconv.Itoa(i)+".mp4")
				if err := recordCmd.Start(); err != nil {
					log.Println("screenrecord error:", err)
					break
				}
			}
			recordDone <- true
		}()
		io.WriteString(w, "screenrecord started")
	}).Methods("POST")

	m.HandleFunc("/screenrecord", func(w http.ResponseWriter, r *http.Request) {
		recordLock.Lock()
		defer recordLock.Unlock()

		recordRunning = false
		if recordCmd != nil {
			if recordCmd.Process != nil {
				recordCmd.Process.Signal(os.Interrupt)
			}
			select {
			case <-recordDone:
			case <-time.After(5 * time.Second):
				// force kill
				exec.Command("pkill", "screenrecord").Run()
			}
			recordCmd = nil
		}
		w.Header().Set("Content-Type", "application/json")
		files, _ := ioutil.ReadDir(recordFolder)
		videos := []string{}
		for i := 0; i < len(files); i++ {
			videos = append(videos, fmt.Sprintf(recordFolder+"%d.mp4", i))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"videos": videos,
		})
	}).Methods("PUT")

	m.HandleFunc("/upgrade", func(w http.ResponseWriter, r *http.Request) {
		ver := r.FormValue("version")
		var err error
		if ver == "" {
			ver, err = getLatestVersion()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		if ver == version {
			io.WriteString(w, "current version is already "+version)
			return
		}
		err = doUpdate(ver)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		io.WriteString(w, "update finished, restarting")
		go func() {
			log.Printf("restarting server")
			runDaemon()
		}()
	})

	m.HandleFunc("/term", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			handleTerminalWebsocket(w, r)
			return
		}
		renderHTML(w, "terminal.html")
	})

	m.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		info := getDeviceInfo()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})

	screenshotFilename := "/data/local/tmp/minicap-screenshot.jpg"
	if username := currentUserName(); username != "" {
		screenshotFilename = "/data/local/tmp/minicap-screenshot-" + username + ".jpg"
	}

	m.HandleFunc("/screenshot", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/screenshot/0", 302)
	}).Methods("GET")

	m.Handle("/jsonrpc/0", uiautomatorProxy)
	m.Handle("/ping", uiautomatorProxy)
	m.HandleFunc("/screenshot/0", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("minicap") == "false" || strings.ToLower(getProperty("ro.product.manufacturer")) == "meizu" {
			uiautomatorProxy.ServeHTTP(w, r)
			return
		}
		if err := Screenshot(screenshotFilename); err != nil {
			log.Printf("screenshot[minicap] error: %v", err)
			uiautomatorProxy.ServeHTTP(w, r)
		} else {
			w.Header().Set("X-Screenshot-Method", "minicap")
			http.ServeFile(w, r, screenshotFilename)
		}
	})

	m.Handle("/assets/{(.*)}", http.StripPrefix("/assets", http.FileServer(Assets)))

	var handler = cors.New(cors.Options{}).Handler(m)
	httpServer = &http.Server{Handler: handler} // url(/stop) need it.
	return httpServer.Serve(lis)
}

func runDaemon() {
	environ := os.Environ()
	// env:IGNORE_SIGHUP forward stdout and stderr to file
	// env:ATX_AGENT will ignore -d flag
	environ = append(environ, "IGNORE_SIGHUP=true", "ATX_AGENT=1")
	cmd := kexec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = environ
	cmd.Start()
	select {
	case err := <-GoFunc(cmd.Wait):
		log.Fatalf("server started failed, %v", err)
	case <-time.After(200 * time.Millisecond):
		fmt.Printf("server started, listening on %v:%d\n", mustGetOoutboundIP(), listenPort)
	}
}

func main() {
	fDaemon := flag.Bool("d", false, "run daemon")
	flag.IntVar(&listenPort, "p", 7912, "listen port") // Create on 2017/09/12
	fVersion := flag.Bool("v", false, "show version")
	fRequirements := flag.Bool("r", false, "install minicap and uiautomator.apk")
	fStop := flag.Bool("stop", false, "stop server")
	fTunnelServer := flag.String("t", "", "tunnel server address")
	fNoUiautomator := flag.Bool("nouia", false, "not start uiautomator")
	flag.Parse()

	if *fVersion {
		fmt.Println(version)
		return
	}

	if *fStop {
		_, err := http.Get("http://127.0.0.1:7912/stop")
		if err != nil {
			log.Println(err)
		} else {
			log.Println("server stopped")
		}
		return
	}

	if *fRequirements {
		log.Println("check dependencies")
		if err := installRequirements(); err != nil {
			// panic(err)
			log.Println("requirements not ready:", err)
			return
		}
	}

	if *fDaemon && os.Getenv("ATX_AGENT") == "" {
		runDaemon()
		return
	}

	if os.Getenv("IGNORE_SIGHUP") == "true" {
		fmt.Println("Enter into daemon mode")
		os.Unsetenv("IGNORE_SIGHUP")

		f, err := os.Create("/sdcard/atx-agent.log")
		if err != nil {
			panic(err)
		}
		defer f.Close()

		os.Stdout = f
		os.Stderr = f
		os.Stdin = nil

		log.SetOutput(f)
		log.Println("Ignore SIGHUP")
		signal.Ignore(syscall.SIGHUP)

		// kill previous daemon first
		log.Println("Kill server")
		_, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/stop", listenPort))
		if err == nil {
			log.Println("wait previous server stopped")
			time.Sleep(1000 * time.Millisecond) // server will quit in 0.1s
		} else {
			log.Println(err)
		}
	}

	fmt.Printf("atx-agent version %s\n", version)

	// show ip
	outIp, err := getOutboundIP()
	if err == nil {
		fmt.Printf("Listen on http://%v:%d\n", outIp, listenPort)
	} else {
		fmt.Printf("Internet is not connected.")
	}

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(listenPort))
	if err != nil {
		log.Fatal(err)
	}

	// minicap + minitouch
	devInfo := getDeviceInfo()
	width, height := devInfo.Display.Width, devInfo.Display.Height
	service.Add("minicap", cmdctrl.CommandInfo{
		Environ: []string{"LD_LIBRARY_PATH=/data/local/tmp"},
		Args: []string{"/data/local/tmp/minicap", "-S", "-P",
			fmt.Sprintf("%dx%d@%dx%d/0", width, height, displayMaxWidthHeight, displayMaxWidthHeight)},
	})
	service.Add("minitouch", cmdctrl.CommandInfo{
		Args: []string{"/data/local/tmp/minitouch"},
	})

	// uiautomator
	service.Add("uiautomator", cmdctrl.CommandInfo{
		Args: []string{"am", "instrument", "-w", "-r",
			"-e", "debug", "false",
			"-e", "class", "com.github.uiautomator.stub.Stub",
			"com.github.uiautomator.test/android.support.test.runner.AndroidJUnitRunner"},
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		MaxRetries:      3,
		RecoverDuration: 30 * time.Second,
	})
	if !*fNoUiautomator {
		if _, err := runShell("am", "start", "-W", "-n", "com.github.uiautomator/.MainActivity"); err != nil {
			log.Println("start uiautomator err:", err)
		}
		if err := service.Start("uiautomator"); err != nil {
			log.Println("uiautomator start failed:", err)
		}
	}

	tunnel := &TunnelProxy{ServerAddr: *fTunnelServer}
	if *fTunnelServer != "" {
		go tunnel.RunForever()
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigc {
			log.Println(sig)
			service.StopAll()
			os.Exit(0)
			httpServer.Shutdown(context.TODO())
		}
	}()
	// run server forever
	if err := ServeHTTP(listener, tunnel); err != nil {
		log.Println("server quit:", err)
	}
}
