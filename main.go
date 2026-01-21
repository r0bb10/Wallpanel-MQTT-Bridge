package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	gpiod "github.com/warthog618/go-gpiocdev"
)

// ============================================================================
// Constants and Configuration
// ============================================================================

// FirmwareVersion is injected at build time via -ldflags
var FirmwareVersion = "dev"

const (
	// ANSI color codes for terminal output
	ColorGreen = "\033[32m"
	ColorReset = "\033[0m"
)

const (
	// Device information for Home Assistant discovery
	Manufacturer  = "r0bb10"
	DeviceName    = "Wallpanel MQTT Bridge"
	HardwareModel = "Raspberry Pi"

	// LIRC ioctl constants for configuring IR transmission hardware
	LIRC_SET_SEND_MODE       = 0x40046911 // Set send mode (LIRC_MODE_PULSE = 0x2)
	LIRC_SET_SEND_CARRIER    = 0x40046913 // Set carrier frequency (e.g., 38kHz)
	LIRC_SET_SEND_DUTY_CYCLE = 0x40046915 // Set duty cycle (typically 33%)
	LIRC_MODE_PULSE          = 0x2        // Send mode: raw pulse/space timings
	bytesPerPulse            = 4          // Bytes per pulse in LIRC format (fixed)
)

// Timing constants
const (
	debounceStabilityTime = 100 * time.Millisecond // Button debounce stability window
	defaultHoldDuration   = 500                     // Default IR hold duration in milliseconds
	irCodeDelay           = 100 * time.Millisecond // Delay between IR code transmissions
	defaultIRGap          = 100000                  // Default gap between IR transmissions in microseconds
)

// ============================================================================
// Configuration Structures
// ============================================================================

// Config is the root configuration structure loaded from JSON
type Config struct {
	MQTT           MQTTConfig           `json:"mqtt"`
	Chip           string               `json:"chip"`              // GPIO chip device (e.g., "gpiochip0")
	Outputs        []OutputConfig       `json:"outputs"`           // GPIO output pins
	Inputs         []InputConfig        `json:"inputs"`            // GPIO input pins
	IRRemotes      []IRRemote           `json:"ir_remotes,omitempty"`
	SystemControls SystemControlsConfig `json:"system_controls,omitempty"`
}

// MQTTConfig defines MQTT broker connection settings
type MQTTConfig struct {
	Broker      string `json:"broker"`       // MQTT broker URL (e.g., "tcp://localhost:1883")
	User        string `json:"user"`         // MQTT username
	Password    string `json:"password"`     // MQTT password
	TopicPrefix string `json:"topic_prefix"` // Base topic for all MQTT messages
}

// OutputConfig defines a GPIO output pin configuration
type OutputConfig struct {
	Name     string `json:"name"`     // Unique identifier for this output
	Pin      int    `json:"pin"`      // GPIO pin number
	Inverted bool   `json:"inverted"`  // If true, LOW=ON and HIGH=OFF
	HAName   string `json:"ha_name"`  // Display name in Home Assistant
	HAClass  string `json:"ha_class"` // Home Assistant device class (e.g., "switch")
	Enabled  *bool  `json:"enabled,omitempty"` // nil or true = enabled, false = disabled
}

// InputConfig defines a GPIO input pin configuration
type InputConfig struct {
	Name         string `json:"name"`          // Unique identifier for this input
	Pin          int    `json:"pin"`           // GPIO pin number
	PullUp       bool   `json:"pullup"`        // Enable internal pull-up resistor
	Inverted     bool   `json:"inverted"`      // If true, LOW=active
	LinkedOutput string `json:"linked_output,omitempty"` // Output to toggle on button press
	HAName       string `json:"ha_name"`       // Display name in Home Assistant
	HAClass      string `json:"ha_class"`       // HA device class (e.g., "motion")
	Enabled      *bool  `json:"enabled,omitempty"` // nil or true = enabled, false = disabled
}

// IRRemote defines an infrared remote control configuration
type IRRemote struct {
	Name      string            `json:"name"`      // Remote identifier
	Device    string            `json:"device"`    // LIRC device path (e.g., "/dev/lirc0")
	Bits      int               `json:"bits"`      // Number of data bits in protocol
	Flags     string            `json:"flags"`     // Protocol flags (e.g., "SPACE_ENC|CONST_LENGTH")
	Header    []int             `json:"header"`    // [mark, space] header pulse timings in microseconds
	One       []int             `json:"one"`       // [mark, space] timings for binary 1
	Zero      []int             `json:"zero"`      // [mark, space] timings for binary 0
	PTrail    int               `json:"ptrail"`    // Trailing pulse in microseconds
	Repeat    []int             `json:"repeat,omitempty"` // [mark, space] for repeat frame
	Gap       int               `json:"gap"`       // Gap between transmissions in microseconds
	Frequency int               `json:"frequency"` // Carrier frequency in Hz (typically 38000)
	Codes     map[string]IRCode `json:"codes"`     // Map of button name to IR code
}

// IRCode defines a single button on a remote
type IRCode struct {
	Code     string `json:"code"`      // Hex code value (e.g., "0x61D66897")
	HAName   string `json:"ha_name"`   // Display name in Home Assistant
	Icon     string `json:"icon,omitempty"` // Material Design Icon
	Enabled  *bool  `json:"enabled,omitempty"` // nil or true = enabled
	SendMode string `json:"send_mode,omitempty"` // "once" or "hold"
	Duration int    `json:"duration,omitempty"`  // Hold duration in milliseconds
}

// SystemControlsConfig defines system-level control buttons
type SystemControlsConfig struct {
	RebootHost      RebootConfig      `json:"reboot_host,omitempty"`
	RestartServices []ServiceConfig   `json:"restart_services,omitempty"`
	CustomCommands  []CustomCommand   `json:"custom_commands,omitempty"`
}

// RebootConfig enables a system reboot button
type RebootConfig struct {
	Enabled bool   `json:"enabled"`
	HAName  string `json:"ha_name"`
	Icon    string `json:"icon,omitempty"`
}

// ServiceConfig defines a systemd service control button
type ServiceConfig struct {
	Service string `json:"service"` // Service name (e.g., "nginx")
	Action  string `json:"action,omitempty"` // Action: restart, reload, start, stop
	HAName  string `json:"ha_name"`
	Icon    string `json:"icon,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// CustomCommand defines a custom shell command button
type CustomCommand struct {
	Name    string `json:"name"`    // Unique identifier
	Command string `json:"command"` // Shell command to execute
	HAName  string `json:"ha_name"`
	Icon    string `json:"icon,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// IRCommandPayload is the MQTT payload structure for IR transmission requests
type IRCommandPayload struct {
	Remote   string   `json:"remote"`   // Remote name
	Send     string   `json:"send"`     // "once" or "hold"
	TimeMsec int      `json:"time_msec,omitempty"` // Duration for hold mode
	Commands []string `json:"commands"` // Button names to send
}

// ============================================================================
// Manager Interfaces and Implementations
// ============================================================================

// Entity represents any Home Assistant entity that can be set up and torn down
type Entity interface {
	// Setup initializes the entity and publishes Home Assistant discovery
	Setup(ctx context.Context, mqtt MQTTManager) error
	// Teardown removes the entity from Home Assistant
	Teardown(mqtt MQTTManager) error
	// Enabled returns whether this entity is enabled
	Enabled() bool
	// Name returns the unique name of this entity
	Name() string
}

// MQTTManager handles all MQTT operations
type MQTTManager interface {
	Publish(topic string, qos byte, retained bool, payload interface{}) error
	Subscribe(topic string, qos byte, handler mqtt.MessageHandler) error
	IsConnected() bool
	TopicPrefix() string
	AvailabilityTopic() string
}

// mqttManager implements MQTTManager using the paho MQTT client
type mqttManager struct {
	client      mqtt.Client
	topicPrefix string
}

// NewMQTTManager creates a new MQTT manager
func NewMQTTManager(client mqtt.Client, topicPrefix string) MQTTManager {
	return &mqttManager{
		client:      client,
		topicPrefix: topicPrefix,
	}
}

func (m *mqttManager) Publish(topic string, qos byte, retained bool, payload interface{}) error {
	var payloadBytes []byte
	switch v := payload.(type) {
	case string:
		payloadBytes = []byte(v)
	case []byte:
		payloadBytes = v
	default:
		var err error
		payloadBytes, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal payload: %w", err)
		}
	}
	token := m.client.Publish(topic, qos, retained, payloadBytes)
	token.Wait()
	return token.Error()
}

func (m *mqttManager) Subscribe(topic string, qos byte, handler mqtt.MessageHandler) error {
	token := m.client.Subscribe(topic, qos, handler)
	token.Wait()
	return token.Error()
}

func (m *mqttManager) IsConnected() bool {
	return m.client.IsConnected()
}

func (m *mqttManager) TopicPrefix() string {
	return m.topicPrefix
}

func (m *mqttManager) AvailabilityTopic() string {
	return fmt.Sprintf("%s/status", m.topicPrefix)
}

// GPIOManager handles all GPIO operations
type GPIOManager interface {
	// OpenChip opens the GPIO chip device
	OpenChip(chipName string) error
	// Close closes the GPIO chip and all lines
	Close() error
	// SetupOutput configures a GPIO pin as output, preserving current state
	SetupOutput(cfg OutputConfig) (*gpiod.Line, error)
	// SetupInput configures a GPIO pin as input with edge detection
	SetupInput(cfg InputConfig, handler func(gpiod.LineEvent)) (*gpiod.Line, error)
	// SetOutput sets an output pin value
	SetOutput(name string, value int) error
	// GetOutput gets an output pin value
	GetOutput(name string) (int, error)
	// ToggleOutput toggles an output pin
	ToggleOutput(name string) error
	// GetOutputConfig returns the cached output config
	GetOutputConfig(name string) (*OutputConfig, bool)
}

// gpioManager implements GPIOManager
type gpioManager struct {
	chip         *gpiod.Chip
	outputLines  map[string]*gpiod.Line
	outputConfigs map[string]*OutputConfig
	inputLines   []*gpiod.Line
	mu           sync.Mutex
}

// NewGPIOManager creates a new GPIO manager
func NewGPIOManager() GPIOManager {
	return &gpioManager{
		outputLines:   make(map[string]*gpiod.Line),
		outputConfigs: make(map[string]*OutputConfig),
		inputLines:    make([]*gpiod.Line, 0),
	}
}

func (g *gpioManager) OpenChip(chipName string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var err error
	g.chip, err = gpiod.NewChip(chipName)
	if err != nil {
		return fmt.Errorf("open chip %s: %w", chipName, err)
	}
	return nil
}

func (g *gpioManager) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var errs []error

	// Close all input lines
	for _, line := range g.inputLines {
		if err := line.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close input line: %w", err))
		}
	}
	g.inputLines = nil

	// Close all output lines
	for name, line := range g.outputLines {
		if err := line.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close output line %s: %w", name, err))
		}
	}
	g.outputLines = make(map[string]*gpiod.Line)
	g.outputConfigs = make(map[string]*OutputConfig)

	// Close chip
	if g.chip != nil {
		if err := g.chip.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close chip: %w", err))
		}
		g.chip = nil
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during close: %v", errs)
	}
	return nil
}

func (g *gpioManager) SetupOutput(cfg OutputConfig) (*gpiod.Line, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.chip == nil {
		return nil, fmt.Errorf("chip not opened")
	}

	// Read current GPIO state to preserve it across restarts
	inputLine, err := g.chip.RequestLine(cfg.Pin, gpiod.AsInput)
	if err != nil {
		return nil, fmt.Errorf("read pin %d state: %w", cfg.Pin, err)
	}

	currentVal, err := inputLine.Value()
	inputLine.Close()
	if err != nil {
		return nil, fmt.Errorf("read pin %d value: %w", cfg.Pin, err)
	}

	// Set as output with the current value to preserve state
	line, err := g.chip.RequestLine(cfg.Pin, gpiod.AsOutput(currentVal))
	if err != nil {
		return nil, fmt.Errorf("request output pin %d: %w", cfg.Pin, err)
	}

	g.outputLines[cfg.Name] = line
	g.outputConfigs[cfg.Name] = &cfg // Cache config for fast lookup

	return line, nil
}

func (g *gpioManager) SetupInput(cfg InputConfig, handler func(gpiod.LineEvent)) (*gpiod.Line, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.chip == nil {
		return nil, fmt.Errorf("chip not opened")
	}

	opts := []gpiod.LineReqOption{
		gpiod.AsInput,
		gpiod.WithBothEdges,
		gpiod.WithEventHandler(handler),
	}

	if cfg.PullUp {
		opts = append(opts, gpiod.WithPullUp)
	}

	line, err := g.chip.RequestLine(cfg.Pin, opts...)
	if err != nil {
		return nil, fmt.Errorf("request input pin %d: %w", cfg.Pin, err)
	}

	g.inputLines = append(g.inputLines, line)
	return line, nil
}

func (g *gpioManager) SetOutput(name string, value int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	line, ok := g.outputLines[name]
	if !ok {
		return fmt.Errorf("output %s not found", name)
	}

	if err := line.SetValue(value); err != nil {
		return fmt.Errorf("set output %s: %w", name, err)
	}
	return nil
}

func (g *gpioManager) GetOutput(name string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	line, ok := g.outputLines[name]
	if !ok {
		return 0, fmt.Errorf("output %s not found", name)
	}

	val, err := line.Value()
	if err != nil {
		return 0, fmt.Errorf("get output %s value: %w", name, err)
	}
	return val, nil
}

func (g *gpioManager) ToggleOutput(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	line, ok := g.outputLines[name]
	if !ok {
		return fmt.Errorf("output %s not found", name)
	}

	currentVal, err := line.Value()
	if err != nil {
		return fmt.Errorf("get output %s value: %w", name, err)
	}

	newVal := 1 - currentVal
	if err := line.SetValue(newVal); err != nil {
		return fmt.Errorf("set output %s: %w", name, err)
	}

	return nil
}

func (g *gpioManager) GetOutputConfig(name string) (*OutputConfig, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	cfg, ok := g.outputConfigs[name]
	return cfg, ok
}

// IRTransmitter handles IR code transmission
type IRTransmitter interface {
	// Transmit sends a single IR code
	Transmit(remote *IRRemote, code uint32) error
	// TransmitHold repeatedly transmits an IR code for a specified duration
	TransmitHold(remote *IRRemote, code uint32, duration int) error
}

// irTransmitter implements IRTransmitter
type irTransmitter struct {
	bufferPool sync.Pool
}

// NewIRTransmitter creates a new IR transmitter
func NewIRTransmitter() IRTransmitter {
	return &irTransmitter{
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 1024)
			},
		},
	}
}

func (t *irTransmitter) Transmit(remote *IRRemote, code uint32) error {
	pulses := t.generatePulses(remote, code)

	if err := t.transmitIR(remote.Device, remote.Frequency, pulses); err != nil {
		return err
	}

	// Optional: wait gap time after transmission
	if remote.Gap > 0 {
		time.Sleep(time.Duration(remote.Gap) * time.Microsecond)
	}
	return nil
}

func (t *irTransmitter) TransmitHold(remote *IRRemote, code uint32, duration int) error {
	if duration <= 0 {
		duration = defaultHoldDuration
	}

	// Step 1: Send the full code once (header + data bits + ptrail)
	pulses := t.generatePulses(remote, code)
	if err := t.transmitIR(remote.Device, remote.Frequency, pulses); err != nil {
		return err
	}

	// Step 2: Wait the gap time after initial transmission
	gap := remote.Gap
	if gap <= 0 {
		gap = defaultIRGap
	}
	time.Sleep(time.Duration(gap) * time.Microsecond)

	// Step 3: Send full code repeatedly for the duration
	// Note: LIRC repeat frames (just header) don't work reliably with all devices,
	// so we send the complete code each time for better compatibility
	deadline := time.Now().Add(time.Duration(duration) * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := t.transmitIR(remote.Device, remote.Frequency, pulses); err != nil {
			return err
		}
		now := time.Now()
		remaining := deadline.Sub(now)
		sleep := time.Duration(gap) * time.Microsecond
		if sleep > remaining {
			sleep = remaining
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
	return nil
}

// generatePulses creates the raw pulse sequence for an IR code.
// Format: header + data bits (MSB first) + trailing pulse.
// Each element represents a pulse duration in microseconds (alternating mark/space).
func (t *irTransmitter) generatePulses(remote *IRRemote, code uint32) []int {
	pulses := []int{}

	// Start with header pulse if defined
	if len(remote.Header) == 2 {
		pulses = append(pulses, remote.Header[0], remote.Header[1])
	}

	// Encode each bit as mark/space pair, MSB first
	for i := remote.Bits - 1; i >= 0; i-- {
		if (code>>i)&1 == 1 {
			// Binary 1: use "one" timing
			pulses = append(pulses, remote.One[0], remote.One[1])
		} else {
			// Binary 0: use "zero" timing
			pulses = append(pulses, remote.Zero[0], remote.Zero[1])
		}
	}

	// Add trailing pulse if defined
	if remote.PTrail > 0 {
		pulses = append(pulses, remote.PTrail)
	}
	return pulses
}

// transmitIR sends raw pulse data to the LIRC device.
// Mimics LIRC behavior: set carrier frequency, then write pulse timings as 32-bit integers.
func (t *irTransmitter) transmitIR(device string, frequency int, pulses []int) error {
	// Open the LIRC device for writing
	fd, err := syscall.Open(device, syscall.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open device %s: %w", device, err)
	}
	defer syscall.Close(fd)

	// Set send mode to LIRC_MODE_PULSE (required by lircd for raw pulse/space timings)
	_, _, _ = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(LIRC_SET_SEND_MODE),
		uintptr(LIRC_MODE_PULSE),
	)

	// Set carrier frequency before transmission (optional, some devices don't support it)
	if frequency > 0 {
		_, _, _ = syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(fd),
			uintptr(LIRC_SET_SEND_CARRIER),
			uintptr(frequency),
		)
		// Note: Many devices don't support LIRC_SET_SEND_CARRIER but still work fine
	}

	// Get a reusable buffer from the pool to reduce allocations
	bufInterface := t.bufferPool.Get()
	buf := bufInterface.([]byte)
	defer t.bufferPool.Put(buf[:0]) // Return empty buffer to pool

	// Ensure buffer is large enough for pulse data
	requiredSize := len(pulses) * bytesPerPulse
	if cap(buf) < requiredSize {
		buf = make([]byte, requiredSize)
	} else {
		buf = buf[:requiredSize]
	}

	// Encode pulses as little-endian 32-bit unsigned integers
	// This is the format LIRC expects
	for i, pulse := range pulses {
		offset := i * bytesPerPulse
		buf[offset] = byte(pulse)        // LSB
		buf[offset+1] = byte(pulse >> 8)
		buf[offset+2] = byte(pulse >> 16)
		buf[offset+3] = byte(pulse >> 24) // MSB
	}

	// Write pulse data to LIRC device
	n, err := syscall.Write(fd, buf)
	if err != nil {
		return fmt.Errorf("write failed after %d bytes: %w", n, err)
	}

	return nil
}

// ============================================================================
// Entity Implementations
// ============================================================================

// OutputEntity represents a GPIO output (switch/relay)
type OutputEntity struct {
	config      OutputConfig
	gpio        GPIOManager
	mqtt        MQTTManager
	line        *gpiod.Line
	commandTopic string
	stateTopic   string
}

// NewOutputEntity creates a new output entity
func NewOutputEntity(cfg OutputConfig, gpio GPIOManager, mqtt MQTTManager) *OutputEntity {
	return &OutputEntity{
		config: cfg,
		gpio:   gpio,
		mqtt:   mqtt,
	}
}

func (e *OutputEntity) Name() string {
	return e.config.Name
}

func (e *OutputEntity) Enabled() bool {
	return isEnabled(e.config.Enabled)
}

func (e *OutputEntity) Setup(ctx context.Context, mqtt MQTTManager) error {
	if !e.Enabled() {
		return nil
	}

	// Setup GPIO output
	line, err := e.gpio.SetupOutput(e.config)
	if err != nil {
		return fmt.Errorf("setup GPIO output: %w", err)
	}
	e.line = line

	// Build topics
	e.commandTopic = fmt.Sprintf("%s/output/%s/set", mqtt.TopicPrefix(), e.config.Name)
	e.stateTopic = fmt.Sprintf("%s/output/%s/state", mqtt.TopicPrefix(), e.config.Name)

	// Publish Home Assistant discovery
	configTopic := fmt.Sprintf("homeassistant/switch/wallpanel_mqtt_bridge/%s/config", e.config.Name)
	availTopic := mqtt.AvailabilityTopic()

	discPayload := discoveryBase(e.config.HAName, fmt.Sprintf("wallpanel_mqtt_bridge_out_%d", e.config.Pin),
		e.commandTopic, e.stateTopic, availTopic)
	discPayload["payload_on"] = "ON"
	discPayload["payload_off"] = "OFF"
	discPayload["optimistic"] = false
	discPayload["retain"] = true
	if e.config.HAClass != "" {
		discPayload["device_class"] = e.config.HAClass
	}

	if err := publishDiscovery(mqtt, configTopic, discPayload, e.config.Name); err != nil {
		return fmt.Errorf("publish discovery: %w", err)
	}

	// Publish initial state
	if err := e.publishState(); err != nil {
		return fmt.Errorf("publish initial state: %w", err)
	}

	// Subscribe to command topic
	handler := e.createCommandHandler()
	if err := mqtt.Subscribe(e.commandTopic, 0, handler); err != nil {
		return fmt.Errorf("subscribe to commands: %w", err)
	}

	return nil
}

func (e *OutputEntity) Teardown(mqtt MQTTManager) error {
	configTopic := fmt.Sprintf("homeassistant/switch/wallpanel_mqtt_bridge/%s/config", e.config.Name)
	return mqtt.Publish(configTopic, 0, true, "")
}

func (e *OutputEntity) publishState() error {
	val, err := e.gpio.GetOutput(e.config.Name)
	if err != nil {
		return err
	}

	isOn := getLogicalState(val, e.config.Inverted)
	state := getStateString(isOn)
	return e.mqtt.Publish(e.stateTopic, 0, true, state)
}

func (e *OutputEntity) createCommandHandler() mqtt.MessageHandler {
	return func(client mqtt.Client, msg mqtt.Message) {
		if msg.Retained() {
			return
		}

		payload := string(msg.Payload())
		targetVal := payloadToGPIOValue(payload, e.config.Inverted)

		if err := e.gpio.SetOutput(e.config.Name, targetVal); err != nil {
			log.Printf("Failed to set output %s: %v", e.config.Name, err)
			return
		}

		log.Printf("%s: %s%s%s", e.config.HAName, ColorGreen, payload, ColorReset)
		if err := e.publishState(); err != nil {
			log.Printf("Failed to publish state for %s: %v", e.config.Name, err)
		}
	}
}

// InputEntity represents a GPIO input (button/sensor)
type InputEntity struct {
	config      InputConfig
	gpio        GPIOManager
	mqtt        MQTTManager
	line        *gpiod.Line
	stateTopic  string
	lastStableState bool
	lastStableTime  time.Time
}

// NewInputEntity creates a new input entity
func NewInputEntity(cfg InputConfig, gpio GPIOManager, mqtt MQTTManager) *InputEntity {
	return &InputEntity{
		config: cfg,
		gpio:   gpio,
		mqtt:   mqtt,
	}
}

func (e *InputEntity) Name() string {
	return e.config.Name
}

func (e *InputEntity) Enabled() bool {
	return isEnabled(e.config.Enabled)
}

func (e *InputEntity) Setup(ctx context.Context, mqtt MQTTManager) error {
	if !e.Enabled() {
		return nil
	}

	// Build topic
	e.stateTopic = fmt.Sprintf("%s/input/%s", mqtt.TopicPrefix(), e.config.Name)

	// Publish Home Assistant discovery
	configTopic := fmt.Sprintf("homeassistant/binary_sensor/wallpanel_mqtt_bridge/%s/config", e.config.Name)
	availTopic := mqtt.AvailabilityTopic()

	discPayload := discoveryBase(e.config.HAName, fmt.Sprintf("wallpanel_mqtt_bridge_in_%d", e.config.Pin),
		"", e.stateTopic, availTopic)
	discPayload["payload_on"] = "ON"
	discPayload["payload_off"] = "OFF"
	if e.config.HAClass != "" {
		discPayload["device_class"] = e.config.HAClass
	}

	if err := publishDiscovery(mqtt, configTopic, discPayload, e.config.Name); err != nil {
		return fmt.Errorf("publish discovery: %w", err)
	}

	// Setup GPIO input with edge detection
	handler := e.createEventHandler()
	line, err := e.gpio.SetupInput(e.config, handler)
	if err != nil {
		return fmt.Errorf("setup GPIO input: %w", err)
	}
	e.line = line

	// Publish initial state
	val, _ := line.Value()
	isPressed := getLogicalState(val, e.config.Inverted)
	e.publishState(isPressed)

	return nil
}

func (e *InputEntity) Teardown(mqtt MQTTManager) error {
	configTopic := fmt.Sprintf("homeassistant/binary_sensor/wallpanel_mqtt_bridge/%s/config", e.config.Name)
	return mqtt.Publish(configTopic, 0, true, "")
}

func (e *InputEntity) createEventHandler() func(gpiod.LineEvent) {
	return func(evt gpiod.LineEvent) {
		now := time.Now()
		isPressed := isPressedFromEdge(evt.Type, e.config.Inverted)

		// Check if state has changed from last stable state
		if isPressed == e.lastStableState {
			return // Bounce - same state, ignore
		}

		// State has potentially changed - check if it's been stable long enough
		if !isStableStateChange(now, e.lastStableTime, debounceStabilityTime) {
			return // Too soon after last stable state change - this is bounce
		}

		// This is a valid state change!
		e.lastStableState = isPressed
		e.lastStableTime = now

		// Log button press only (with friendly name)
		if isPressed {
			log.Printf("%s: %sPRESSED%s", e.config.HAName, ColorGreen, ColorReset)
		}

		// Publish state to MQTT
		e.publishState(isPressed)

		// ONLY TOGGLE ON PRESS (when button goes active)
		if isPressed && e.config.LinkedOutput != "" {
			if err := e.gpio.ToggleOutput(e.config.LinkedOutput); err != nil {
				log.Printf("Error toggling linked output %s: %v", e.config.LinkedOutput, err)
				return
			}

			// Get output config to log with friendly name
			cfg, ok := e.gpio.GetOutputConfig(e.config.LinkedOutput)
			if ok {
				val, _ := e.gpio.GetOutput(e.config.LinkedOutput)
				logicalState := getStateString(getLogicalState(val, cfg.Inverted))
				log.Printf("%s: toggled %s%s%s", cfg.HAName, ColorGreen, logicalState, ColorReset)
			}
		}
	}
}

func (e *InputEntity) publishState(isPressed bool) {
	state := getStateString(isPressed)
	e.mqtt.Publish(e.stateTopic, 0, true, state)
}

// IRButtonEntity represents an IR remote button
type IRButtonEntity struct {
	remoteName string
	codeName   string
	code       IRCode
	remote     *IRRemote
	ir         IRTransmitter
	mqtt       MQTTManager
}

// NewIRButtonEntity creates a new IR button entity
func NewIRButtonEntity(remoteName string, codeName string, code IRCode, remote *IRRemote, ir IRTransmitter, mqtt MQTTManager) *IRButtonEntity {
	return &IRButtonEntity{
		remoteName: remoteName,
		codeName:   codeName,
		code:       code,
		remote:     remote,
		ir:         ir,
		mqtt:       mqtt,
	}
}

func (e *IRButtonEntity) Name() string {
	return fmt.Sprintf("ir_%s_%s", e.remoteName, e.codeName)
}

func (e *IRButtonEntity) Enabled() bool {
	return isEnabled(e.code.Enabled)
}

func (e *IRButtonEntity) Setup(ctx context.Context, mqtt MQTTManager) error {
	if !e.Enabled() {
		return nil
	}

	// Determine send mode and duration
	sendMode := e.code.SendMode
	if sendMode == "" {
		sendMode = "once"
	}
	duration := e.code.Duration
	if duration == 0 && sendMode == "hold" {
		duration = defaultHoldDuration
	}

	// Build entity name
	entityName := e.code.HAName
	if entityName == "" {
		entityName = fmt.Sprintf("%s %s", e.remoteName, e.codeName)
	}

	// Build button press payload
	pressPayload := map[string]interface{}{
		"remote":   e.remoteName,
		"send":     sendMode,
		"commands": []string{e.codeName},
	}
	if duration > 0 {
		pressPayload["time_msec"] = duration
	}

	pressJSON, err := json.Marshal(pressPayload)
	if err != nil {
		return fmt.Errorf("marshal press payload: %w", err)
	}

	// Publish Home Assistant discovery
	configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/ir_%s_%s/config",
		strings.ToLower(e.remoteName), strings.ToLower(e.codeName))
	uniqueID := fmt.Sprintf("wallpanel_mqtt_bridge_ir_%s_%s", e.remoteName, e.codeName)
	irTopic := fmt.Sprintf("%s/ir/send", mqtt.TopicPrefix())
	availTopic := mqtt.AvailabilityTopic()

	discPayload := discoveryBase(entityName, uniqueID, irTopic, "", availTopic)
	discPayload["payload_press"] = string(pressJSON)
	if e.code.Icon != "" {
		discPayload["icon"] = e.code.Icon
	}

	return publishDiscovery(mqtt, configTopic, discPayload, fmt.Sprintf("%s/%s", e.remoteName, e.codeName))
}

func (e *IRButtonEntity) Teardown(mqtt MQTTManager) error {
	configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/ir_%s_%s/config",
		strings.ToLower(e.remoteName), strings.ToLower(e.codeName))
	return mqtt.Publish(configTopic, 0, true, "")
}

// SystemButtonEntity represents a system control button (reboot, service, custom command)
type SystemButtonEntity struct {
	entityType string // "reboot", "service", "custom"
	name       string
	haName     string
	icon       string
	commandTopic string
	handler    mqtt.MessageHandler
}

// NewRebootButtonEntity creates a reboot button entity
func NewRebootButtonEntity(cfg RebootConfig, mqtt MQTTManager) *SystemButtonEntity {
	rebootName := cfg.HAName
	if rebootName == "" {
		rebootName = "Reboot Wallpanel"
	}
	rebootTopic := fmt.Sprintf("%s/system/reboot", mqtt.TopicPrefix())
	icon := withDefaultIcon(cfg.Icon, "mdi:restart")

	return &SystemButtonEntity{
		entityType:   "reboot",
		name:         "system_reboot",
		haName:       rebootName,
		icon:         icon,
		commandTopic: rebootTopic,
		handler:      handleRebootCommand(rebootName),
	}
}

// NewServiceButtonEntity creates a service control button entity
func NewServiceButtonEntity(cfg ServiceConfig, mqtt MQTTManager) *SystemButtonEntity {
	action := cfg.Action
	if action == "" {
		action = "restart"
	}

	serviceTopic := fmt.Sprintf("%s/system/%s/%s", mqtt.TopicPrefix(), action, cfg.Service)
	safeName := strings.ReplaceAll(cfg.Service, ".", "_")
	serviceName := cfg.HAName
	if serviceName == "" {
		actionTitle := strings.ToUpper(action[:1]) + strings.ToLower(action[1:])
		serviceName = fmt.Sprintf("%s %s", actionTitle, cfg.Service)
	}
	icon := withDefaultIcon(cfg.Icon, "mdi:restart-alert")

	return &SystemButtonEntity{
		entityType:   "service",
		name:         fmt.Sprintf("system_%s_%s", action, safeName),
		haName:       serviceName,
		icon:         icon,
		commandTopic: serviceTopic,
		handler:      handleServiceCommand(action, cfg.Service, cfg.HAName),
	}
}

// NewCustomCommandButtonEntity creates a custom command button entity
func NewCustomCommandButtonEntity(cfg CustomCommand, mqtt MQTTManager) *SystemButtonEntity {
	cmdTopic := fmt.Sprintf("%s/system/custom/%s", mqtt.TopicPrefix(), cfg.Name)
	safeName := strings.ReplaceAll(cfg.Name, ".", "_")
	safeName = strings.ReplaceAll(safeName, " ", "_")
	cmdHAName := cfg.HAName
	if cmdHAName == "" {
		cmdHAName = fmt.Sprintf("Custom: %s", cfg.Name)
	}
	icon := withDefaultIcon(cfg.Icon, "mdi:console")

	return &SystemButtonEntity{
		entityType:   "custom",
		name:         fmt.Sprintf("system_custom_%s", safeName),
		haName:       cmdHAName,
		icon:         icon,
		commandTopic: cmdTopic,
		handler:      handleCustomCommand(cfg.Command, cfg.HAName),
	}
}

func (e *SystemButtonEntity) Name() string {
	return e.name
}

func (e *SystemButtonEntity) Enabled() bool {
	return true // System buttons are always enabled if created
}

func (e *SystemButtonEntity) Setup(ctx context.Context, mqtt MQTTManager) error {
	configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/%s/config", e.name)
	availTopic := mqtt.AvailabilityTopic()

	discPayload := discoveryBase(e.haName, fmt.Sprintf("wallpanel_mqtt_bridge_%s", e.name),
		e.commandTopic, "", availTopic)
	discPayload["payload_press"] = "EXECUTE"
	discPayload["entity_category"] = "diagnostic"
	discPayload["icon"] = e.icon

	if err := publishDiscovery(mqtt, configTopic, discPayload, e.name); err != nil {
		return fmt.Errorf("publish discovery: %w", err)
	}

	// Subscribe to command topic
	return mqtt.Subscribe(e.commandTopic, 0, e.handler)
}

func (e *SystemButtonEntity) Teardown(mqtt MQTTManager) error {
	configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/%s/config", e.name)
	return mqtt.Publish(configTopic, 0, true, "")
}

// ============================================================================
// Entity Manager
// ============================================================================

// EntityManager manages the lifecycle of all entities
type EntityManager struct {
	entities []Entity
	mu       sync.Mutex
}

// NewEntityManager creates a new entity manager
func NewEntityManager() *EntityManager {
	return &EntityManager{
		entities: make([]Entity, 0),
	}
}

// Register adds an entity to the manager
func (em *EntityManager) Register(entity Entity) {
	em.mu.Lock()
	defer em.mu.Unlock()
	em.entities = append(em.entities, entity)
}

// SetupAll initializes all enabled entities
func (em *EntityManager) SetupAll(ctx context.Context, mqtt MQTTManager) error {
	em.mu.Lock()
	defer em.mu.Unlock()

	var errs []error
	for _, entity := range em.entities {
		if !entity.Enabled() {
			continue
		}

		if err := entity.Setup(ctx, mqtt); err != nil {
			errs = append(errs, fmt.Errorf("setup %s: %w", entity.Name(), err))
			// Continue with other entities even if one fails
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during setup: %v", errs)
	}
	return nil
}

// TeardownAll removes all entities from Home Assistant
func (em *EntityManager) TeardownAll(mqtt MQTTManager) error {
	em.mu.Lock()
	defer em.mu.Unlock()

	var errs []error
	for _, entity := range em.entities {
		if err := entity.Teardown(mqtt); err != nil {
			errs = append(errs, fmt.Errorf("teardown %s: %w", entity.Name(), err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during teardown: %v", errs)
	}
	return nil
}

// Clear removes all entities from the manager
func (em *EntityManager) Clear() {
	em.mu.Lock()
	defer em.mu.Unlock()
	em.entities = make([]Entity, 0)
}

// ============================================================================
// Helper Functions
// ============================================================================

// isEnabled checks if an entity is enabled (nil or true means enabled)
func isEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

// withDefaultIcon returns icon if provided, otherwise defaultIcon
func withDefaultIcon(icon, defaultIcon string) string {
	if icon != "" {
		return icon
	}
	return defaultIcon
}

// payloadToGPIOValue converts MQTT payload ("ON"/"OFF") to GPIO value considering inversion
func payloadToGPIOValue(payload string, inverted bool) int {
	isOn := payload == "ON"
	if inverted {
		if isOn {
			return 0
		}
		return 1
	}
	if isOn {
		return 1
	}
	return 0
}

// getStateString converts a boolean state to "ON" or "OFF" string
func getStateString(isOn bool) string {
	if isOn {
		return "ON"
	}
	return "OFF"
}

// getLogicalState converts a GPIO pin value to logical state considering inversion
func getLogicalState(pinVal int, inverted bool) bool {
	return (pinVal == 1 && !inverted) || (pinVal == 0 && inverted)
}

// isPressedFromEdge determines if button is pressed based on edge type and inversion
func isPressedFromEdge(evtType gpiod.LineEventType, inverted bool) bool {
	if inverted {
		return evtType == gpiod.LineEventFallingEdge
	}
	return evtType == gpiod.LineEventRisingEdge
}

// isStableStateChange checks if enough time has passed since last stable state change
func isStableStateChange(now, lastStableTime time.Time, debounceTime time.Duration) bool {
	return now.Sub(lastStableTime) >= debounceTime
}

// getDeviceInfo returns the device information payload for Home Assistant discovery
func getDeviceInfo() map[string]interface{} {
	return map[string]interface{}{
		"identifiers":  []string{"wallpanel_mqtt_bridge"},
		"name":         DeviceName,
		"manufacturer": Manufacturer,
		"model":        HardwareModel,
		"sw_version":   FirmwareVersion,
	}
}

// discoveryBase creates a base discovery payload with common fields
func discoveryBase(name, uniqueID, commandTopic, stateTopic, availabilityTopic string) map[string]interface{} {
	payload := map[string]interface{}{
		"name":               name,
		"unique_id":          uniqueID,
		"availability_topic": availabilityTopic,
		"device":             getDeviceInfo(),
	}
	if commandTopic != "" {
		payload["command_topic"] = commandTopic
	}
	if stateTopic != "" {
		payload["state_topic"] = stateTopic
	}
	return payload
}

// publishDiscovery publishes a discovery payload to Home Assistant
func publishDiscovery(mqtt MQTTManager, configTopic string, payload map[string]interface{}, entityName string) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discovery payload for %s: %w", entityName, err)
	}
	return mqtt.Publish(configTopic, 0, true, jsonPayload)
}

// handleRebootCommand returns an MQTT message handler for reboot commands
func handleRebootCommand(rebootName string) mqtt.MessageHandler {
	return func(client mqtt.Client, msg mqtt.Message) {
		if string(msg.Payload()) == "EXECUTE" {
			if err := exec.Command("sudo", "reboot").Run(); err != nil {
				log.Printf("Error executing reboot: %v", err)
			} else {
				log.Printf("System %s: %sDONE%s", rebootName, ColorGreen, ColorReset)
			}
		}
	}
}

// handleServiceCommand returns an MQTT message handler for service control commands
func handleServiceCommand(action, serviceName, haName string) mqtt.MessageHandler {
	return func(client mqtt.Client, msg mqtt.Message) {
		if string(msg.Payload()) == "EXECUTE" {
			if err := exec.Command("sudo", "systemctl", action, serviceName).Run(); err != nil {
				log.Printf("Error executing service %s %s: %v", action, serviceName, err)
			} else {
				log.Printf("Service %s: %sDONE%s", haName, ColorGreen, ColorReset)
			}
		}
	}
}

// handleCustomCommand returns an MQTT message handler for custom shell commands
func handleCustomCommand(command, haName string) mqtt.MessageHandler {
	return func(client mqtt.Client, msg mqtt.Message) {
		if string(msg.Payload()) == "EXECUTE" {
			// Execute command through bash -c for environment variable expansion
			if err := exec.Command("bash", "-c", command).Run(); err != nil {
				log.Printf("Error executing custom command %s: %v", haName, err)
			} else {
				log.Printf("Custom command %s: %sDONE%s", haName, ColorGreen, ColorReset)
			}
		}
	}
}

// isValidServiceName validates service names to prevent command injection
func isValidServiceName(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// ============================================================================
// Application Main
// ============================================================================

// Application represents the main application state
type Application struct {
	config       Config
	configFile   string
	mqttClient   mqtt.Client
	mqttManager  MQTTManager
	gpioManager  GPIOManager
	irTransmitter IRTransmitter
	entityManager *EntityManager
	irRemotes    map[string]*IRRemote
	mu           sync.Mutex
}

// NewApplication creates a new application instance
func NewApplication(configFile string) (*Application, error) {
	cfg, err := loadConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	app := &Application{
		config:        cfg,
		configFile:    configFile,
		gpioManager:   NewGPIOManager(),
		irTransmitter: NewIRTransmitter(),
		entityManager: NewEntityManager(),
		irRemotes:     make(map[string]*IRRemote),
	}

	return app, nil
}

// InitializeMQTT connects to MQTT broker and sets up connection handler
func (app *Application) InitializeMQTT() error {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(app.config.MQTT.Broker)
	opts.SetUsername(app.config.MQTT.User)
	opts.SetPassword(app.config.MQTT.Password)
	opts.SetClientID("wallpanel-mqtt-bridge")
	opts.SetAutoReconnect(true)

	availTopic := fmt.Sprintf("%s/status", app.config.MQTT.TopicPrefix)
	opts.SetWill(availTopic, "offline", 0, true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Printf("%sConnected%s to MQTT", ColorGreen, ColorReset)
		c.Publish(availTopic, 0, true, "online")

		app.mu.Lock()
		if err := app.initializeHardware(c); err != nil {
			log.Printf("Error initializing hardware: %v", err)
		}
		app.mu.Unlock()
	})

	app.mqttClient = mqtt.NewClient(opts)
	if token := app.mqttClient.Connect(); token.Wait() && token.Error() != nil {
		return fmt.Errorf("MQTT connection failed: %w", token.Error())
	}

	app.mqttManager = NewMQTTManager(app.mqttClient, app.config.MQTT.TopicPrefix)
	return nil
}

// initializeHardware sets up all hardware and entities
func (app *Application) initializeHardware(c mqtt.Client) error {
	// Close existing GPIO resources before reinitializing (in case of MQTT reconnect)
	if err := app.gpioManager.Close(); err != nil {
		log.Printf("Warning: error closing existing GPIO: %v", err)
	}

	// Clear existing entities
	app.entityManager.Clear()

	// Build IR remotes map
	app.irRemotes = make(map[string]*IRRemote)
	for i := range app.config.IRRemotes {
		remote := &app.config.IRRemotes[i]
		app.irRemotes[remote.Name] = remote
	}

	// Setup IR command handler (shared topic for all IR buttons)
	irTopic := fmt.Sprintf("%s/ir/send", app.config.MQTT.TopicPrefix)
	app.mqttManager.Subscribe(irTopic, 0, app.createIRCommandHandler())

	// Open GPIO chip if needed
	if len(app.config.Outputs) > 0 || len(app.config.Inputs) > 0 {
		if err := app.gpioManager.OpenChip(app.config.Chip); err != nil {
			log.Printf("Error opening GPIO chip: %v", err)
			// Continue without GPIO - IR and system controls still work
		}
	}

	// Register all entities
	app.registerEntities()

	// Setup all enabled entities
	ctx := context.Background()
	if err := app.entityManager.SetupAll(ctx, app.mqttManager); err != nil {
		return fmt.Errorf("setup entities: %w", err)
	}

	return nil
}

// registerEntities creates and registers all entities from config
func (app *Application) registerEntities() {
	app.entityManager.Clear()

	// Register output entities
	for _, cfg := range app.config.Outputs {
		entity := NewOutputEntity(cfg, app.gpioManager, app.mqttManager)
		app.entityManager.Register(entity)
	}

	// Register input entities
	for _, cfg := range app.config.Inputs {
		entity := NewInputEntity(cfg, app.gpioManager, app.mqttManager)
		app.entityManager.Register(entity)
	}

	// Register IR button entities
	for _, remote := range app.config.IRRemotes {
		for codeName, code := range remote.Codes {
			entity := NewIRButtonEntity(remote.Name, codeName, code, &remote, app.irTransmitter, app.mqttManager)
			app.entityManager.Register(entity)
		}
	}

	// Register system control entities
	if app.config.SystemControls.RebootHost.Enabled {
		entity := NewRebootButtonEntity(app.config.SystemControls.RebootHost, app.mqttManager)
		app.entityManager.Register(entity)
	}

	for _, svc := range app.config.SystemControls.RestartServices {
		if !isEnabled(svc.Enabled) {
			continue
		}

		action := svc.Action
		if action == "" {
			action = "restart"
		}

		// Validate action
		validActions := map[string]bool{"restart": true, "reload": true, "start": true, "stop": true}
		if !validActions[action] {
			log.Printf("Security Warning: Invalid action '%s' for service %s", action, svc.Service)
			continue
		}

		// Validate service name
		if !isValidServiceName(svc.Service) {
			log.Printf("Security Warning: Skipping invalid service name: %s", svc.Service)
			continue
		}

		entity := NewServiceButtonEntity(svc, app.mqttManager)
		app.entityManager.Register(entity)
	}

	for _, cmd := range app.config.SystemControls.CustomCommands {
		if !isEnabled(cmd.Enabled) {
			continue
		}

		if cmd.Name == "" || cmd.Command == "" {
			log.Printf("Security Warning: Skipping custom command with empty name or command")
			continue
		}

		entity := NewCustomCommandButtonEntity(cmd, app.mqttManager)
		app.entityManager.Register(entity)
	}
}

// createIRCommandHandler creates the handler for IR command messages
func (app *Application) createIRCommandHandler() mqtt.MessageHandler {
	return func(client mqtt.Client, msg mqtt.Message) {
		var payload IRCommandPayload
		if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
			log.Printf("Error unmarshaling IR command: %v", err)
			return
		}

		type irTask struct {
			Remote *IRRemote
			CodeVal uint32
		}
		var tasks []irTask

		app.mu.Lock()
		remote, exists := app.irRemotes[payload.Remote]
		if exists {
			for _, cmdName := range payload.Commands {
				code, ok := remote.Codes[cmdName]
				if !ok {
					continue
				}
				if !isEnabled(code.Enabled) {
					continue
				}

				// Parse hex code string (supports "0x" prefix or plain hex)
				codeStr := strings.TrimPrefix(code.Code, "0x")
				codeStr = strings.TrimPrefix(codeStr, "0X")
				codeVal, err := strconv.ParseUint(codeStr, 16, 32)
				if err != nil {
					log.Printf("Error parsing IR code '%s': %v", code.Code, err)
					continue
				}

				// Log command with friendly name
				log.Printf("Sent %sIR%s: %s%s%s", ColorGreen, ColorReset, ColorGreen, code.HAName, ColorReset)

				tasks = append(tasks, irTask{Remote: remote, CodeVal: uint32(codeVal)})
			}
		}
		app.mu.Unlock()

		// Transmit each IR code with appropriate mode
		for _, task := range tasks {
			var err error
			if payload.Send == "hold" {
				dur := payload.TimeMsec
				if dur == 0 {
					dur = defaultHoldDuration
				}
				err = app.irTransmitter.TransmitHold(task.Remote, task.CodeVal, dur)
			} else {
				err = app.irTransmitter.Transmit(task.Remote, task.CodeVal)
			}
			if err != nil {
				log.Printf("Error sending IR: %v", err)
			}
			time.Sleep(irCodeDelay)
		}
	}
}

// Reload reloads configuration from disk
func (app *Application) Reload() error {
	app.mu.Lock()
	defer app.mu.Unlock()

	newConfig, err := loadConfig(app.configFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Teardown all current entities
	if app.mqttManager != nil && app.mqttManager.IsConnected() {
		app.entityManager.TeardownAll(app.mqttManager)
	}

	// Shutdown hardware
	app.gpioManager.Close()

	// Update config
	app.config = newConfig

	// Reinitialize if MQTT is connected
	if app.mqttManager != nil && app.mqttManager.IsConnected() {
		if err := app.initializeHardware(app.mqttClient); err != nil {
			return fmt.Errorf("reinitialize hardware: %w", err)
		}
	}

	log.Println("Configuration reload complete.")
	return nil
}

// Shutdown gracefully shuts down the application
func (app *Application) Shutdown() error {
	app.mu.Lock()
	defer app.mu.Unlock()

	// Teardown all entities
	if app.mqttManager != nil {
		app.entityManager.TeardownAll(app.mqttManager)
	}

	// Publish offline status
	if app.mqttManager != nil {
		availTopic := app.mqttManager.AvailabilityTopic()
		app.mqttManager.Publish(availTopic, 0, true, "offline")
	}

	// Shutdown hardware
	if err := app.gpioManager.Close(); err != nil {
		log.Printf("Error closing GPIO: %v", err)
	}

	// Disconnect MQTT
	if app.mqttClient != nil {
		app.mqttClient.Disconnect(250)
	}

	return nil
}

// loadConfig reads and parses the JSON configuration file
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file: %w", err)
	}
	return cfg, nil
}

// main is the entry point of the application
func main() {
	log.Printf("Wallpanel MQTT Bridge v%s", FirmwareVersion)

	// Determine configuration file path from command line or use default
	configFile := "config.json"
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}

	// Create application
	app, err := NewApplication(configFile)
	if err != nil {
		log.Fatalf("Critical: %v", err)
	}

	// Initialize MQTT connection
	if err := app.InitializeMQTT(); err != nil {
		log.Fatalf("MQTT initialization failed: %v", err)
	}

	// Setup signal handling for graceful shutdown and config reload
	log.Println("Running. Press Ctrl+C to exit, or send SIGHUP to reload config.")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	for {
		s := <-sig
		if s == syscall.SIGHUP {
			log.Println("Received SIGHUP - Reloading configuration...")
			if err := app.Reload(); err != nil {
				log.Printf("Reload failed: %v", err)
			}
		} else {
			break
		}
	}

	// Graceful shutdown
	log.Println("Shutting down...")
	if err := app.Shutdown(); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
}

