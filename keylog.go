package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	DefaultEndpoint           = "https://example.com/wh/keylog"
	DefaultDevice             = "default"
	DefaultReportingFrequency = 12
)

var (
	endpoint  = flag.String("endpoint", DefaultEndpoint, "The endpoint to post keylog data to.")
	device    = flag.String("device", DefaultDevice, "The name of this device.")
	frequency = flag.Int("frequency", DefaultReportingFrequency, "The frequency of reports (hours).")
)

func main() {
	flag.Parse()
	if *endpoint == DefaultEndpoint {
		log.Fatalf("reporting endpoint not set, terminating")
	}
	path, err := FindDevicePath()
	if err != nil {
		log.Fatalf("failed to resolve keyboard device: %v", err)
	}
	logger, err := NewKeylogger(path)
	if err != nil {
		log.Fatalf("failed to create keylogger: %v", err)
	}
	go func() {
		for {
			st := time.Duration(*frequency) * time.Hour
			time.Sleep(st)
			if err := logger.Report(); err != nil {
				log.Printf("failed to report: %v", err)
			}
		}
	}()
	if err := logger.Start(); err != nil {
		log.Printf("logger unexpectedly terminated: %v", err)
	}
}

// note that this will return the first keyboard device, does not support multiple keyboards
func FindDevicePath() (string, error) {
	path, resolved := "/sys/class/input/event%d/device/name", "/dev/input/event%d"
	for i := 0; i < 255; i++ {
		buf, err := ioutil.ReadFile(fmt.Sprintf(path, i))
		if err != nil {
			return "", err
		}
		if strings.Contains(strings.ToLower(string(buf)), "keyboard") {
			log.Printf("using keyboard: %d", i)
			return fmt.Sprintf(resolved, i), nil
		}
	}
	return "", errors.New("not found")
}

type Keylogger struct {
	sync.Mutex
	count int64
	fd    *os.File
}

func IsRoot() bool {
	return syscall.Getuid() == 0 && syscall.Geteuid() == 0
}

func NewKeylogger(devpath string) (*Keylogger, error) {
	if !IsRoot() {
		return nil, errors.New("must be run as root")
	}
	fd, err := os.Open(devpath)
	if err != nil {
		return nil, err
	}
	return &Keylogger{fd: fd}, nil
}

type KeylogReport struct {
	Keystrokes int64  `json:"keystrokes"`
	DeviceName string `json:"deviceName"`
	Frequency  int    `json:"frequency"`
}

func (k *Keylogger) Report() error {
	k.Lock()
	defer k.Unlock()
	report := KeylogReport{
		Keystrokes: k.count,
		DeviceName: *device,
		Frequency:  *frequency,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	resp, err := http.Post(*endpoint, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("got non-ok status code %d", resp.StatusCode)
	}
	k.count = 0 // reset the count after we verify that server returned 200
	return nil
}

type InputEvent struct {
	Time  syscall.Timeval
	Type  uint16
	Code  uint16
	Value int32
}

func (i *InputEvent) IsKeyPress() bool {
	return i.Value == 1
}

func (k *Keylogger) Start() error {
	bufsize := int(unsafe.Sizeof(InputEvent{}))
	buf, event := make([]byte, bufsize), &InputEvent{}
	for {
		n, err := k.fd.Read(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			time.Sleep(time.Millisecond * 20)
			continue
		}
		log.Printf("N=%d", n)
		err = binary.Read(bytes.NewBuffer(buf), binary.LittleEndian, event)
		if err != nil {
			return err
		}
		if event.IsKeyPress() {
			k.Lock()
			k.count++
			log.Println("pressed")
			k.Unlock()
		}
		log.Println("here")
		buf, event = buf[:0], &InputEvent{}
	}
}
