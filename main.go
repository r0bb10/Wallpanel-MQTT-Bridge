package main

import (
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

// FirmwareVersion is injected at build time via -ldflags
var FirmwareVersion = "dev"

const (
	ColorGreen = "\033[32m"
	ColorReset = "\033[0m"
)

const (
	Manufacturer  = "r0bb10"
	DeviceName    = "Wallpanel MQTT Bridge"
	HardwareModel = "Raspberry Pi"
	
	// LIRC ioctl constants for configuring IR transmission hardware
	LIRC_SET_SEND_MODE       = 0x40046911 // Set send mode (LIRC_MODE_PULSE = 0x2)
	LIRC_SET_SEND_CARRIER    = 0x40046913 // Set carrier frequency (e.g., 38kHz)
	LIRC_SET_SEND_DUTY_CYCLE = 0x40046915 // Set duty cycle (typically 33%)
	LIRC_MODE_PULSE          = 0x2        // Send mode: raw pulse/space timings
)

// Configuration structures

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
	Inverted bool   `json:"inverted"` // If true, LOW=ON and HIGH=OFF
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
	HAClass      string `json:"ha_class"`      // HA device class (e.g., "motion")
	Enabled      *bool  `json:"enabled,omitempty"` // nil or true = enabled, false = disabled
}

// IRRemote defines an infrared remote control configuration (from LIRC)
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

// Global state management
var (
	activeInputs []*gpiod.Line              // Active GPIO input lines with edge detection
	outputLines  = make(map[string]*gpiod.Line) // Map of output names to GPIO lines
	appConfig    Config                      // Current application configuration
	mqttClient   mqtt.Client                 // MQTT client connection
	gpioChip     *gpiod.Chip                 // GPIO chip handle
	irRemotes    map[string]*IRRemote        // Map of IR remote configurations
	configFile   string                      // Path to configuration file
	mu           sync.Mutex                  // Protects concurrent access to GPIO and config
	irBufferPool = sync.Pool{               // Reusable buffers for IR transmission
		New: func() interface{} {
			return make([]byte, 0, 1024)
		},
	}
)

func main() {
	log.Printf("Wallpanel MQTT Bridge v%s", FirmwareVersion)

	// Determine configuration file path from command line or use default
	configFile = "config.json"
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}

	// Load and validate configuration
	var err error
	appConfig, err = loadConfig(configFile)
	if err != nil {
		log.Fatalf("Critical: %v", err)
	}

	// Configure MQTT client with authentication and last will testament
	opts := mqtt.NewClientOptions()
	opts.AddBroker(appConfig.MQTT.Broker)
	opts.SetUsername(appConfig.MQTT.User)
	opts.SetPassword(appConfig.MQTT.Password)
	opts.SetClientID("wallpanel-mqtt-bridge")
	opts.SetAutoReconnect(true)
	availTopic := fmt.Sprintf("%s/status", appConfig.MQTT.TopicPrefix)
	opts.SetWill(availTopic, "offline", 0, true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Printf("%sConnected%s to MQTT", ColorGreen, ColorReset)
		c.Publish(availTopic, 0, true, "online")
		mu.Lock()
		initializeHardware(c)
		mu.Unlock()
	})

	// Establish MQTT connection
	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("MQTT connection failed: %v", token.Error())
	}

	// Setup signal handling for graceful shutdown and config reload
	log.Println("Running. Press Ctrl+C to exit, or send SIGHUP to reload config.")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	for {
		s := <-sig
		if s == syscall.SIGHUP {
			log.Println("Received SIGHUP - Reloading configuration...")
			handleReload()
		} else {
			break
		}
	}

	// Graceful shutdown: cleanup hardware, publish offline status, disconnect MQTT
	log.Println("Shutting down...")
	mu.Lock()
	shutdownHardware()
	mqttClient.Publish(availTopic, 0, true, "offline")
	mqttClient.Disconnect(250)
	mu.Unlock()
}

// initializeHardware sets up GPIO, IR remotes, and system controls with Home Assistant discovery

func initializeHardware(c mqtt.Client) {
	irRemotes = make(map[string]*IRRemote)
	for i := range appConfig.IRRemotes {
		remote := &appConfig.IRRemotes[i]
		irRemotes[remote.Name] = remote
	}

	if len(appConfig.Outputs) > 0 || len(appConfig.Inputs) > 0 {
		var err error
		gpioChip, err = gpiod.NewChip(appConfig.Chip)
		if err != nil {
			log.Printf("Error opening chip %s: %v", appConfig.Chip, err)
		} else {
			setupOutputs(c)
			setupInputs(c)
		}
	}

	if gpioChip != nil {
		subscribeToCommands(c)
	}
	setupIRButtons(c)
	setupSystemButtons(c)
}

func shutdownHardware() {
	for _, l := range activeInputs {
		l.Close()
	}
	activeInputs = nil
	for _, l := range outputLines {
		l.Close()
	}
	outputLines = make(map[string]*gpiod.Line)
	if gpioChip != nil {
		gpioChip.Close()
		gpioChip = nil
	}
}

// handleReload reloads configuration from disk and reinitializes hardware

func handleReload() {
	mu.Lock()
	defer mu.Unlock()

	newConfig, err := loadConfig(configFile)
	if err != nil {
		log.Printf("Reload aborted: %v", err)
		return
	}
	
	// Remove disabled entities from Home Assistant before reinitializing
	cleanupEntities(&appConfig, &newConfig)
	shutdownHardware()
	appConfig = newConfig
	if mqttClient.IsConnected() {
		initializeHardware(mqttClient)
	}
	log.Println("Configuration reload complete.")
}

// loadConfig reads and parses the JSON configuration file

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("error reading config file: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("error parsing config file: %v", err)
	}
	return cfg, nil
}

// cleanupEntities removes Home Assistant entities that were disabled or removed from config

func cleanupEntities(oldCfg, newCfg *Config) {
	// Track which outputs are still enabled in new config
	newOutputs := make(map[string]bool)
	for _, o := range newCfg.Outputs {
		if o.Enabled == nil || *o.Enabled {
			newOutputs[o.Name] = true
		}
	}
	for _, o := range oldCfg.Outputs {
		if !newOutputs[o.Name] {
			removeHAEntity(fmt.Sprintf("homeassistant/switch/wallpanel_mqtt_bridge/%s/config", o.Name))
		}
	}

	// Track which inputs are still enabled in new config
	newInputs := make(map[string]bool)
	for _, i := range newCfg.Inputs {
		if i.Enabled == nil || *i.Enabled {
			newInputs[i.Name] = true
		}
	}
	for _, i := range oldCfg.Inputs {
		if !newInputs[i.Name] {
			removeHAEntity(fmt.Sprintf("homeassistant/binary_sensor/wallpanel_mqtt_bridge/%s/config", i.Name))
		}
	}

	// Check IR remote codes for removal
	for _, oldRemote := range oldCfg.IRRemotes {
		var newRemote *IRRemote
		for _, nr := range newCfg.IRRemotes {
			if nr.Name == oldRemote.Name {
				r := nr
				newRemote = &r
				break
			}
		}
		for codeName := range oldRemote.Codes {
			shouldRemove := false
			if newRemote == nil {
				shouldRemove = true
			} else {
				newCode, exists := newRemote.Codes[codeName]
				if !exists {
					shouldRemove = true
				} else if newCode.Enabled != nil && !*newCode.Enabled {
					shouldRemove = true
				}
			}
			if shouldRemove {
				removeHAEntity(fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/ir_%s_%s/config",
					strings.ToLower(oldRemote.Name), strings.ToLower(codeName)))
			}
		}
	}

	// Remove reboot button if disabled
	if oldCfg.SystemControls.RebootHost.Enabled && !newCfg.SystemControls.RebootHost.Enabled {
		removeHAEntity("homeassistant/button/wallpanel_mqtt_bridge/system_reboot/config")
	}

	// Track which service controls are still enabled
	newSvcs := make(map[string]bool)
	for _, s := range newCfg.SystemControls.RestartServices {
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		act := s.Action
		if act == "" {
			act = "restart"
		}
		key := fmt.Sprintf("%s_%s", act, s.Service)
		newSvcs[key] = true
	}

	for _, s := range oldCfg.SystemControls.RestartServices {
		act := s.Action
		if act == "" {
			act = "restart"
		}
		key := fmt.Sprintf("%s_%s", act, s.Service)
		if !newSvcs[key] {
			safeName := strings.ReplaceAll(s.Service, ".", "_")
			removeHAEntity(fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/system_%s_%s/config", act, safeName))
		}
	}
}

// removeHAEntity publishes an empty retained message to remove a Home Assistant entity

func removeHAEntity(topic string) {
	log.Printf("Removing HA Entity: %s", topic)
	mqttClient.Publish(topic, 0, true, "")
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

// setupOutputs configures GPIO output pins and publishes Home Assistant discovery configs
func setupOutputs(c mqtt.Client) {
	availTopic := fmt.Sprintf("%s/status", appConfig.MQTT.TopicPrefix)
	for _, out := range appConfig.Outputs {
		if out.Enabled != nil && !*out.Enabled {
			continue
		}

		// Read current GPIO state to preserve it across restarts
		inputLine, err := gpioChip.RequestLine(out.Pin, gpiod.AsInput)
		if err != nil {
			log.Printf("ERROR: Cannot read pin %d state: %v - SKIPPING", out.Pin, err)
			continue
		}
		
		currentVal, err := inputLine.Value()
		inputLine.Close()
		if err != nil {
			log.Printf("ERROR: Cannot read pin %d value: %v - SKIPPING", out.Pin, err)
			continue
		}

		// Set as output with the current value to preserve state
		line, err := gpioChip.RequestLine(out.Pin, gpiod.AsOutput(currentVal))
		if err != nil {
			log.Printf("ERROR: Cannot request output pin %d: %v", out.Pin, err)
			continue
		}
		outputLines[out.Name] = line

		// Publish Home Assistant MQTT discovery configuration
		configTopic := fmt.Sprintf("homeassistant/switch/wallpanel_mqtt_bridge/%s/config", out.Name)
		discPayload := map[string]interface{}{
			"name":              out.HAName,
			"unique_id":         fmt.Sprintf("wallpanel_mqtt_bridge_out_%d", out.Pin),
			"command_topic":     fmt.Sprintf("%s/output/%s/set", appConfig.MQTT.TopicPrefix, out.Name),
			"state_topic":       fmt.Sprintf("%s/output/%s/state", appConfig.MQTT.TopicPrefix, out.Name),
			"availability_topic": availTopic,
			"payload_on":        "ON",
			"payload_off":       "OFF",
			"optimistic":        false,
			"retain":            true,
			"device":            getDeviceInfo(),
		}
		if out.HAClass != "" {
			discPayload["device_class"] = out.HAClass
		}

		jsonPayload, err := json.Marshal(discPayload)
		if err != nil {
			log.Printf("Error marshaling discovery payload for output %s: %v", out.Name, err)
			continue
		}
		c.Publish(configTopic, 0, true, jsonPayload)
		publishOutputState(c, out, line)
	}
}

// publishOutputState reads the GPIO line value and publishes the current state to MQTT
func publishOutputState(c mqtt.Client, cfg OutputConfig, line *gpiod.Line) {
	pinVal, err := line.Value()
	if err != nil {
		return
	}
	state := "OFF"
	if (pinVal == 1 && !cfg.Inverted) || (pinVal == 0 && cfg.Inverted) {
		state = "ON"
	}
	stateTopic := fmt.Sprintf("%s/output/%s/state", appConfig.MQTT.TopicPrefix, cfg.Name)
	c.Publish(stateTopic, 0, true, state)
}

// subscribeToCommands subscribes to MQTT command topics for all configured outputs
func subscribeToCommands(c mqtt.Client) {
	for _, out := range appConfig.Outputs {
		if out.Enabled != nil && !*out.Enabled {
			continue
		}
		topic := fmt.Sprintf("%s/output/%s/set", appConfig.MQTT.TopicPrefix, out.Name)
		outCfg := out
		c.Subscribe(topic, 0, func(client mqtt.Client, msg mqtt.Message) {
			handleCommand(client, msg, outCfg)
		})
	}
}

func handleCommand(c mqtt.Client, msg mqtt.Message, cfg OutputConfig) {
	if msg.Retained() {
		return
	}
	payload := string(msg.Payload())

	// Determine target value based on payload and inversion setting
	targetVal := 0
	if payload == "ON" {
		if cfg.Inverted {
			targetVal = 0
		} else {
			targetVal = 1
		}
	} else {
		if cfg.Inverted {
			targetVal = 1
		} else {
			targetVal = 0
		}
	}

	// Set GPIO and publish state while holding mutex to avoid races with reload
	mu.Lock()
	line, ok := outputLines[cfg.Name]
	if ok {
		if err := line.SetValue(targetVal); err == nil {
			// Log the state change with friendly name and color
			logicalState := "OFF"
			if payload == "ON" {
				logicalState = "ON"
			}
			log.Printf("%s: %s%s%s", cfg.HAName, ColorGreen, logicalState, ColorReset)
			publishOutputState(c, cfg, line)
		} else {
			log.Printf("Failed to set output %s: %v", cfg.Name, err)
		}
	}
	mu.Unlock()
}

func setupInputs(c mqtt.Client) {
	availTopic := fmt.Sprintf("%s/status", appConfig.MQTT.TopicPrefix)
	for _, in := range appConfig.Inputs {
		if in.Enabled != nil && !*in.Enabled { continue }

		configTopic := fmt.Sprintf("homeassistant/binary_sensor/wallpanel_mqtt_bridge/%s/config", in.Name)
		discPayload := map[string]interface{}{
			"name":              in.HAName,
			"unique_id":         fmt.Sprintf("wallpanel_mqtt_bridge_in_%d", in.Pin),
			"state_topic":       fmt.Sprintf("%s/input/%s", appConfig.MQTT.TopicPrefix, in.Name),
			"availability_topic": availTopic,
			"payload_on":        "ON",
			"payload_off":       "OFF",
			"device":            getDeviceInfo(),
		}
		if in.HAClass != "" {
			discPayload["device_class"] = in.HAClass
		}

		jsonPayload, err := json.Marshal(discPayload)
		if err != nil {
			log.Printf("Error marshaling discovery payload for input %s: %v", in.Name, err)
			continue
		}
		c.Publish(configTopic, 0, true, jsonPayload)

		line := monitorInput(in)
		if line != nil {
			activeInputs = append(activeInputs, line)
		}
	}
}

// monitorInput sets up edge detection on a GPIO input pin - SIMPLIFIED VERSION
func monitorInput(cfg InputConfig) *gpiod.Line {
	var lastStableState bool
	var lastStableTime time.Time
	var line *gpiod.Line
	stabilityTime := 100 * time.Millisecond // State must be stable for 100ms before acting
	
	handler := func(evt gpiod.LineEvent) {
		now := time.Now()
		
		// Determine if button is pressed based on edge type
		var isPressed bool
		if cfg.Inverted {
			isPressed = (evt.Type == gpiod.LineEventFallingEdge)
		} else {
			isPressed = (evt.Type == gpiod.LineEventRisingEdge)
		}

		// Read current value for logging (commented out for production)
		// val, _ := line.Value()
		// relayState := "N/A"
		// if cfg.LinkedOutput != "" {
		// 	mu.Lock()
		// 	if relayLine, ok := outputLines[cfg.LinkedOutput]; ok {
		// 		if relayVal, err := relayLine.Value(); err == nil {
		// 			relayState = fmt.Sprintf("%d", relayVal)
		// 		}
		// 	}
		// 	mu.Unlock()
		// }

		// Check if state has changed from last stable state
		if isPressed == lastStableState {
			// Bounce - same state, ignore
			// log.Printf(">>> BOUNCE IGNORED: %s | edge=%v, same as stable state (pressed=%v)", cfg.Name, evt.Type, isPressed)
			return
		}

		// State has potentially changed - check if it's been stable long enough
		timeSinceLastStable := now.Sub(lastStableTime)
		if timeSinceLastStable < stabilityTime {
			// Too soon after last stable state change - this is bounce
			// log.Printf(">>> TOO SOON: %s | edge=%v, only %vms since last stable change", cfg.Name, evt.Type, timeSinceLastStable.Milliseconds())
			return
		}

		// This is a valid state change!
		lastStableState = isPressed
		lastStableTime = now

		// Log button press only (with friendly name)
		if isPressed {
			log.Printf("%s: %sPRESSED%s", cfg.HAName, ColorGreen, ColorReset)
		}

		// Publish state to MQTT
		state := "OFF"
		if isPressed {
			state = "ON"
		}
		topic := fmt.Sprintf("%s/input/%s", appConfig.MQTT.TopicPrefix, cfg.Name)
		mqttClient.Publish(topic, 0, true, state)

		// ONLY TOGGLE ON PRESS (when button goes active)
		if isPressed && cfg.LinkedOutput != "" {
			toggleLinkedOutput(cfg.LinkedOutput)
		}
	}

	// Configure GPIO line as input with pull-up
	opts := []gpiod.LineReqOption{
		gpiod.AsInput,
		gpiod.WithBothEdges,
		gpiod.WithEventHandler(handler),
	}
	
	if cfg.PullUp {
		opts = append(opts, gpiod.WithPullUp)
	}

	var err error
	line, err = gpioChip.RequestLine(cfg.Pin, opts...)
	if err != nil {
		log.Printf("ERROR: Failed to request input pin %d: %v", cfg.Pin, err)
		return nil
	}

	// Read and log initial state
	val, _ := line.Value()
	isPressed := (val == 0 && cfg.Inverted) || (val == 1 && !cfg.Inverted)

	// Publish initial state
	state := "OFF"
	if isPressed {
		state = "ON"
	}
	topic := fmt.Sprintf("%s/input/%s", appConfig.MQTT.TopicPrefix, cfg.Name)
	mqttClient.Publish(topic, 0, true, state)

	return line
}

// toggleLinkedOutput toggles a GPIO output when triggered by a linked input
func toggleLinkedOutput(outputName string) {
	mu.Lock()
	defer mu.Unlock()
	
	line, ok := outputLines[outputName]
	if !ok {
		log.Printf("ERROR: Output '%s' not found in outputLines map", outputName)
		return
	}

	var outCfg OutputConfig
	found := false
	for _, o := range appConfig.Outputs {
		if o.Name == outputName {
			outCfg = o
			found = true
			break
		}
	}
	if !found {
		log.Printf("ERROR: Output config '%s' not found", outputName)
		return
	}

	// Read current GPIO value
	currentVal, err := line.Value()
	if err != nil {
		log.Printf("ERROR: Can't read output '%s': %v", outputName, err)
		return
	}
	
	// Toggle: 0->1 or 1->0
	newVal := 1 - currentVal
	
	if err := line.SetValue(newVal); err != nil {
		log.Printf("ERROR: Can't set output '%s' to %d: %v", outputName, newVal, err)
		return
	}
	
	// Log the toggle with friendly name and colored logical state
	logicalState := "OFF"
	if (newVal == 1 && !outCfg.Inverted) || (newVal == 0 && outCfg.Inverted) {
		logicalState = "ON"
	}
	log.Printf("%s: toggled %s%s%s", outCfg.HAName, ColorGreen, logicalState, ColorReset)
	
	publishOutputState(mqttClient, outCfg, line)
}

// setupIRButtons creates Home Assistant button entities for IR remote codes
func setupIRButtons(c mqtt.Client) {
	availTopic := fmt.Sprintf("%s/status", appConfig.MQTT.TopicPrefix)
	irTopic := fmt.Sprintf("%s/ir/send", appConfig.MQTT.TopicPrefix)

	c.Subscribe(irTopic, 0, handleIRCommand)

	for remoteName, remote := range irRemotes {
		for codeName, code := range remote.Codes {
			if code.Enabled != nil && !*code.Enabled {
				continue
			}

			// Determine send mode and duration for IR transmission
			sendMode := code.SendMode
			if sendMode == "" {
				sendMode = "once"
			}
			duration := code.Duration
			if duration == 0 && sendMode == "hold" {
				duration = 500
			}

			configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/ir_%s_%s/config",
				strings.ToLower(remoteName), strings.ToLower(codeName))

			uniqueID := fmt.Sprintf("wallpanel_mqtt_bridge_ir_%s_%s", remoteName, codeName)
			entityName := code.HAName
			if entityName == "" {
				entityName = fmt.Sprintf("%s %s", remoteName, codeName)
			}

			// Build button press payload with IR command details
			pressPayload := map[string]interface{}{
				"remote":   remoteName,
				"send":     sendMode,
				"commands": []string{codeName},
			}
			if duration > 0 {
				pressPayload["time_msec"] = duration
			}

			pressJSON, err := json.Marshal(pressPayload)
			if err != nil {
				log.Printf("Error marshaling IR press payload for %s/%s: %v", remoteName, codeName, err)
				continue
			}

			discPayload := map[string]interface{}{
				"name":              entityName,
				"unique_id":         uniqueID,
				"command_topic":     irTopic,
				"payload_press":     string(pressJSON),
				"availability_topic": availTopic,
				"device":            getDeviceInfo(),
			}
			if code.Icon != "" {
				discPayload["icon"] = code.Icon
			}

			jsonPayload, err := json.Marshal(discPayload)
			if err != nil {
				log.Printf("Error marshaling discovery payload for IR %s/%s: %v", remoteName, codeName, err)
				continue
			}
			c.Publish(configTopic, 0, true, jsonPayload)
		}
	}
}

// handleIRCommand processes MQTT messages to send IR codes
func handleIRCommand(client mqtt.Client, msg mqtt.Message) {
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

	mu.Lock()
	remote, exists := irRemotes[payload.Remote]
	if exists {
		for _, cmdName := range payload.Commands {
			code, ok := remote.Codes[cmdName]
			if !ok { continue }
			if code.Enabled != nil && !*code.Enabled { continue }
			
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
	mu.Unlock()

	// Transmit each IR code with appropriate mode
	for _, task := range tasks {
		var err error
		if payload.Send == "hold" {
			dur := payload.TimeMsec
			if dur == 0 {
				dur = 500
			}
			err = sendIRCodeHold(task.Remote, task.CodeVal, dur)
		} else {
			err = sendIRCode(task.Remote, task.CodeVal)
		}
		if err != nil {
			log.Printf("Error sending IR: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sendIRCode transmits a single IR code (for "once" mode)
func sendIRCode(remote *IRRemote, code uint32) error {
	// Generate the pulse sequence from the code
	pulses := generatePulses(remote, code)
	
	// Transmit via LIRC device with carrier frequency
	if err := transmitIR(remote.Device, remote.Frequency, pulses); err != nil {
		return err
	}
	
	// Optional: wait gap time after transmission
	if remote.Gap > 0 {
		time.Sleep(time.Duration(remote.Gap) * time.Microsecond)
	}
	return nil
}

// sendIRCodeHold repeatedly transmits an IR code for a specified duration.
// This mimics LIRC behavior: send full code once, then send repeat frames.
func sendIRCodeHold(remote *IRRemote, code uint32, duration int) error {
	if duration <= 0 {
		duration = 500
	}
	
	// Step 1: Send the full code once (header + data bits + ptrail)
	pulses := generatePulses(remote, code)
	if err := transmitIR(remote.Device, remote.Frequency, pulses); err != nil {
		return err
	}
	
	// Step 2: Wait the gap time after initial transmission
	gap := remote.Gap
	if gap <= 0 {
		gap = 100000 // Default 100ms gap if not specified
	}
	time.Sleep(time.Duration(gap) * time.Microsecond)
	
	// Step 3: Send full code repeatedly for the duration
	// Note: LIRC repeat frames (just header) don't work reliably with all devices,
	// so we send the complete code each time for better compatibility
	deadline := time.Now().Add(time.Duration(duration) * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := transmitIR(remote.Device, remote.Frequency, pulses); err != nil {
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
func generatePulses(remote *IRRemote, code uint32) []int {
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
func transmitIR(device string, frequency int, pulses []int) error {
	// log.Printf("IR TX: Opening device %s for %d pulses at %dHz", device, len(pulses), frequency)
	
	// Open the LIRC device for writing
	fd, err := syscall.Open(device, syscall.O_WRONLY, 0)
	if err != nil {
		log.Printf("IR TX ERROR: Failed to open %s: %v", device, err)
		return err
	}
	defer syscall.Close(fd)
	// log.Printf("IR TX: Device opened successfully, fd=%d", fd)

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
		// ret, _, errno := syscall.Syscall(...)
		// if errno != 0 {
		// 	log.Printf("IR TX WARNING: ioctl LIRC_SET_SEND_CARRIER failed: errno=%v (ret=%d)", errno, ret)
		// 	log.Printf("IR TX: Continuing without carrier frequency setting (some devices don't support it)")
		// } else {
		// 	log.Printf("IR TX: Carrier frequency set to %dHz", frequency)
		// }
	}

	// Get a reusable buffer from the pool to reduce allocations
	bufInterface := irBufferPool.Get()
	buf := bufInterface.([]byte)
	defer irBufferPool.Put(buf[:0]) // Return empty buffer to pool

	// Ensure buffer is large enough for pulse data (4 bytes per pulse)
	requiredSize := len(pulses) * 4
	if cap(buf) < requiredSize {
		buf = make([]byte, requiredSize)
	} else {
		buf = buf[:requiredSize]
	}

	// Encode pulses as little-endian 32-bit unsigned integers
	// This is the format LIRC expects
	for i, pulse := range pulses {
		buf[i*4] = byte(pulse)        // LSB
		buf[i*4+1] = byte(pulse >> 8)
		buf[i*4+2] = byte(pulse >> 16)
		buf[i*4+3] = byte(pulse >> 24) // MSB
	}
	
	// Write pulse data to LIRC device
	n, err := syscall.Write(fd, buf)
	if err != nil {
		log.Printf("IR TX ERROR: Write failed after %d bytes: %v", n, err)
		return err
	}
	
	// log.Printf("IR TX: Successfully wrote %d bytes", n)
	return nil
}

// setupSystemButtons creates Home Assistant button entities for system control functions
func setupSystemButtons(c mqtt.Client) {
	availTopic := fmt.Sprintf("%s/status", appConfig.MQTT.TopicPrefix)

	// Setup reboot button if enabled
	if appConfig.SystemControls.RebootHost.Enabled {
		rebootTopic := fmt.Sprintf("%s/system/reboot", appConfig.MQTT.TopicPrefix)
		configTopic := "homeassistant/button/wallpanel_mqtt_bridge/system_reboot/config"
		rebootName := appConfig.SystemControls.RebootHost.HAName
		if rebootName == "" {
			rebootName = "Reboot Wallpanel"
		}
		discPayload := map[string]interface{}{
			"name":              rebootName,
			"unique_id":         "wallpanel_mqtt_bridge_system_reboot",
			"command_topic":     rebootTopic,
			"payload_press":     "REBOOT",
			"availability_topic": availTopic,
			"device":            getDeviceInfo(),
			"entity_category":   "diagnostic",
			"icon":              "mdi:restart",
		}
		if appConfig.SystemControls.RebootHost.Icon != "" {
			discPayload["icon"] = appConfig.SystemControls.RebootHost.Icon
		}
		jsonPayload, err := json.Marshal(discPayload)
		if err != nil {
			log.Printf("Error marshaling reboot button discovery: %v", err)
		} else {
			c.Publish(configTopic, 0, true, jsonPayload)
		}

		c.Subscribe(rebootTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
			if string(msg.Payload()) == "REBOOT" {
				exec.Command("sudo", "reboot").Run()
				log.Printf("System %s: %sDONE%s", rebootName, ColorGreen, ColorReset)
			}
		})
	}

	// Setup service control buttons
	for _, svc := range appConfig.SystemControls.RestartServices {
		if svc.Enabled != nil && !*svc.Enabled {
			continue
		}

		action := svc.Action
		if action == "" {
			action = "restart"
		}

		validActions := map[string]bool{"restart": true, "reload": true, "start": true, "stop": true}
		if !validActions[action] {
			log.Printf("Security Warning: Invalid action '%s' for service %s", action, svc.Service)
			continue
		}
		if !isValidServiceName(svc.Service) {
			log.Printf("Security Warning: Skipping invalid service name: %s", svc.Service)
			continue
		}

		serviceTopic := fmt.Sprintf("%s/system/%s/%s", appConfig.MQTT.TopicPrefix, action, svc.Service)
		safeName := strings.ReplaceAll(svc.Service, ".", "_")
		configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/system_%s_%s/config", action, safeName)

		serviceName := svc.HAName
		if serviceName == "" {
			serviceName = fmt.Sprintf("%s %s", strings.Title(action), svc.Service)
		}
		discPayload := map[string]interface{}{
			"name":              serviceName,
			"unique_id":         fmt.Sprintf("wallpanel_mqtt_bridge_system_%s_%s", action, safeName),
			"command_topic":     serviceTopic,
			"payload_press":     "EXECUTE",
			"availability_topic": availTopic,
			"device":            getDeviceInfo(),
			"entity_category":   "diagnostic",
			"icon":              "mdi:restart-alert",
		}
		if svc.Icon != "" {
			discPayload["icon"] = svc.Icon
		}

		jsonPayload, err := json.Marshal(discPayload)
		if err != nil {
			log.Printf("Error marshaling service button discovery for %s: %v", svc.Service, err)
			continue
		}
		c.Publish(configTopic, 0, true, jsonPayload)

		svcName := svc.Service
		actName := action
		svcHAName := svc.HAName
		c.Subscribe(serviceTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
			if string(msg.Payload()) == "EXECUTE" {
				exec.Command("sudo", "systemctl", actName, svcName).Run()
				log.Printf("Service %s: %sDONE%s", svcHAName, ColorGreen, ColorReset)
			}
		})
	}

	// Setup custom command buttons
	for _, cmd := range appConfig.SystemControls.CustomCommands {
		if cmd.Enabled != nil && !*cmd.Enabled {
			continue
		}

		if cmd.Name == "" || cmd.Command == "" {
			log.Printf("Security Warning: Skipping custom command with empty name or command")
			continue
		}

		cmdTopic := fmt.Sprintf("%s/system/custom/%s", appConfig.MQTT.TopicPrefix, cmd.Name)
		safeName := strings.ReplaceAll(cmd.Name, ".", "_")
		safeName = strings.ReplaceAll(safeName, " ", "_")
		configTopic := fmt.Sprintf("homeassistant/button/wallpanel_mqtt_bridge/system_custom_%s/config", safeName)

		cmdHAName := cmd.HAName
		if cmdHAName == "" {
			cmdHAName = fmt.Sprintf("Custom: %s", cmd.Name)
		}

		discPayload := map[string]interface{}{
			"name":              cmdHAName,
			"unique_id":         fmt.Sprintf("wallpanel_mqtt_bridge_system_custom_%s", safeName),
			"command_topic":     cmdTopic,
			"payload_press":     "EXECUTE",
			"availability_topic": availTopic,
			"device":            getDeviceInfo(),
			"entity_category":   "diagnostic",
			"icon":              "mdi:console",
		}
		if cmd.Icon != "" {
			discPayload["icon"] = cmd.Icon
		}

		jsonPayload, err := json.Marshal(discPayload)
		if err != nil {
			log.Printf("Error marshaling custom command button discovery for %s: %v", cmd.Name, err)
			continue
		}
		c.Publish(configTopic, 0, true, jsonPayload)

		cmdCommand := cmd.Command
		cmdName := cmd.HAName
		c.Subscribe(cmdTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
			if string(msg.Payload()) == "EXECUTE" {
				// Execute command through bash -c for environment variable expansion
				exec.Command("bash", "-c", cmdCommand).Run()
				log.Printf("Custom command %s: %sDONE%s", cmdName, ColorGreen, ColorReset)
			}
		})
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