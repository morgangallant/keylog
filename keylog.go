package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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
	DefaultReportingFrequency = 1
)

var (
	endpoint  = flag.String("endpoint", DefaultEndpoint, "The endpoint to post keylog data to.")
	device    = flag.String("device", DefaultDevice, "The name of this device.")
	frequency = flag.Int("frequency", DefaultReportingFrequency, "The frequency of reports (hours).")
)

func FindDevices() []string {
	path, resolved := "/sys/class/input/event%d/device/name", "/dev/input/event%d"
	devices := []string{}
	for i := 0; i < 255; i++ {
		buf, err := ioutil.ReadFile(fmt.Sprintf(path, i))
		if err != nil {
			break
		}
		if strings.Contains(strings.ToLower(string(buf)), "keyboard") {
			devices = append(devices, fmt.Sprintf(resolved, i))
		}
	}
	return devices
}

func IsRoot() bool {
	return syscall.Getuid() == 0 && syscall.Geteuid() == 0
}

func main() {
	flag.Parse()
	if *endpoint == DefaultEndpoint {
		log.Fatalf("reporting endpoint not set, terminating")
	}
	if !IsRoot() {
		log.Fatalln("must be root to run the keylogger")
	}
	devices := FindDevices()
	if len(devices) == 0 {
		log.Fatalln("zero input devices found, terminating")
	}
	logger, err := NewKeylogger(devices)
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
	if err := logger.Run(); err != nil {
		log.Printf("logger unexpectedly terminated: %v", err)
	}
}

type Timestamp struct {
	Seconds      uint64
	Microseconds uint64
}

type InputEvent struct {
	Time  Timestamp
	Type  uint16
	Code  uint16
	Value int32
}

func (i *InputEvent) IsKeyPress() bool {
	return i.Value == 1
}

var InputEventSize = int(unsafe.Sizeof(InputEvent{}))

type DeviceLogger struct {
	fd   *os.File
	out  chan InputEvent
	done chan struct{}
}

func NewDeviceLogger(device string, out chan InputEvent) (*DeviceLogger, error) {
	fd, err := os.OpenFile(device, os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &DeviceLogger{
		fd:   fd,
		out:  out,
		done: make(chan struct{}),
	}, nil
}

func (d *DeviceLogger) Run() {
	event, buffer := InputEvent{}, make([]byte, InputEventSize)
	for {
		n, err := d.fd.Read(buffer)
		if err != nil {
			return
		}
		if n != InputEventSize {
			continue
		}
		event.Time.Seconds = binary.LittleEndian.Uint64(buffer[0:8])
		event.Time.Microseconds = binary.LittleEndian.Uint64(buffer[8:16])
		event.Type = binary.LittleEndian.Uint16(buffer[16:18])
		event.Code = binary.LittleEndian.Uint16(buffer[18:20])
		event.Value = int32(binary.LittleEndian.Uint32(buffer[20:24]))
		select {
		case <-d.done:
			return
		case d.out <- event:
			// fallthrough
		}
	}
}

func (d *DeviceLogger) Finish() error {
	close(d.done)
	return d.fd.Close()
}

type Keylogger struct {
	mu       sync.Mutex
	count    int64
	incoming chan InputEvent
	loggers  []*DeviceLogger
}

func NewKeylogger(devices []string) (*Keylogger, error) {
	loggers, out := []*DeviceLogger{}, make(chan InputEvent, 1)
	for _, device := range devices {
		logger, err := NewDeviceLogger(device, out)
		if err != nil {
			return nil, err
		}
		loggers = append(loggers, logger)
	}
	return &Keylogger{incoming: out, loggers: loggers}, nil
}

type KeylogReport struct {
	Keystrokes int64  `json:"keystrokes"`
	DeviceName string `json:"deviceName"`
	Frequency  int    `json:"frequency"`
}

func (k *Keylogger) Report() error {
	k.mu.Lock()
	defer k.mu.Unlock()
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

func (k *Keylogger) Run() error {
	for _, logger := range k.loggers {
		go logger.Run()
	}
	for event := range k.incoming {
		if !event.IsKeyPress() {
			continue
		}
		k.mu.Lock()
		k.count++
		k.mu.Unlock()
	}
	for _, logger := range k.loggers {
		if err := logger.Finish(); err != nil {
			return err
		}
	}
	return nil
}

func (k *Keylogger) Finish() {
	close(k.incoming)
}
