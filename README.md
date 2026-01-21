# Wallpanel MQTT Bridge

A unified MQTT bridge for Raspberry Pi that exposes GPIO, IR remote control, and system management to Home Assistant with automatic discovery.

## Features

- **GPIO Control**: Control outputs and monitor inputs via MQTT
- **IR Remote Control**: Send infrared commands
- **Linked Outputs**: Toggle relays directly from GPIO buttons
- **System Management**: Reboot host and restart/reload services from Home Assistant
- **Home Assistant Integration**: Automatic discovery - all entities appear instantly
- **Reliable**: Auto-reconnect, availability tracking, proper cleanup
- **Efficient**: Native Go, single binary, low resource usage

## Quick Install

1. **Download the latest release** for your architecture:
   - ARM64 (Pi 4/5): `wallpanel-mqtt-bridge-linux-arm64.tar.gz`
   - ARMv7 (Pi 3/Zero 2/W): `wallpanel-mqtt-bridge-linux-armv7.tar.gz`

2. **Extract and install**:
   ```bash
   tar -xzf wallpanel-mqtt-bridge-linux-*.tar.gz
   cd wallpanel-mqtt-bridge-linux-*
   sudo ./install.sh
   ```

3. **Configure** (edit `/opt/wallpanel-mqtt-bridge/config.json`):
   ```bash
   sudo nano /opt/wallpanel-mqtt-bridge/config.json
   ```

4. **Start the service**:
   ```bash
   sudo systemctl enable wallpanel-mqtt-bridge
   sudo systemctl start wallpanel-mqtt-bridge
   ```

5. **Check status**:
   ```bash
   sudo systemctl status wallpanel-mqtt-bridge
   sudo journalctl -u wallpanel-mqtt-bridge -f
   ```

## Configuration

Edit `/opt/wallpanel-mqtt-bridge/config.json`:

```json
{
  "mqtt": {
    "broker": "tcp://mqtt_host:1883",
    "user": "mqtt_user",
    "password": "mqtt_password",
    "topic_prefix": "wallpanel/wallpanel-mqtt-bridge"
  },
  "chip": "gpiochip0",
  "outputs": [
    {
      "name": "relay1",
      "pin": 23,
      "inverted": true,
      "ha_name": "Display",
      "ha_class": "switch",
      "enabled": true
    }
  ],
  "inputs": [
    {
      "name": "button1",
      "pin": 26,
      "pullup": true,
      "inverted": true,
      "linked_output": "relay1",
      "ha_name": "Button",
      "enabled": true
    }
  ],
  "ir_remotes": [
    {
      "name": "UPERFECT",
      "device": "/dev/lirc0",
      "bits": 32,
      "flags": "SPACE_ENC|CONST_LENGTH",
      "header": [9117, 4434],
      "one": [642, 1624],
      "zero": [642, 488],
      "ptrail": 638,
      "repeat": [9102, 2199],
      "gap": 80000,
      "frequency": 38000,
      "codes": {
        "KEY_BRIGHTNESSUP": {
          "code": "0x61D66897",
          "ha_name": "Brightness Up",
          "icon": "mdi:brightness-5"
        },
        "KEY_BRIGHTNESSUP_MAX": {
          "code": "0x61D66897",
          "send_mode": "hold",
          "duration": 3000,
          "ha_name": "Brightness Max",
          "icon": "mdi:brightness-7"
        },
        "KEY_BRIGHTNESSDOWN": {
          "code": "0x61D628D7",
          "ha_name": "Brightness Down",
          "icon": "mdi:brightness-4"
        },
        "KEY_BRIGHTNESSDOWN_MIN": {
          "code": "0x61D628D7",
          "send_mode": "hold",
          "duration": 3000,
          "ha_name": "Brightness Min",
          "icon": "mdi:brightness-1"
        },
        "KEY_POWER": {
          "code": "0x61D608F7",
          "enabled": false,
          "ha_name": "Power",
          "icon": "mdi:power"
        },
        "KEY_MUTE": {
          "code": "0x61D648B7",
          "enabled": false,
          "ha_name": "Mute",
          "icon": "mdi:volume-mute"
        }
      }
    }
  ],
  "system_controls": {
    "reboot_host": {
      "enabled": true,
      "ha_name": "Reboot Wallpanel",
      "icon": "mdi:restart"
    },
    "restart_services": [
      {
        "service": "wallpanel-mqtt-bridge",
        "action": "reload",
        "enabled": false,
        "ha_name": "Reload Bridge",
        "icon": "mdi:refresh"
      },
      {
        "service": "greetd",
        "action": "restart",
        "enabled": true,
        "ha_name": "Restart Kiosk",
        "icon": "mdi:monitor-dashboard"
      }
    ]
  }
}
```

### Configuration Options

**MQTT Settings:**
- `broker`: MQTT broker URL (format: `tcp://host:port`)
- `user`: MQTT username
- `password`: MQTT password
- `topic_prefix`: Base topic for all messages

**GPIO Chip:**
- `chip`: GPIO chip device name (usually `gpiochip0`).

**GPIO Output (Relay/Switch) Settings:**
- `name`: Internal name (used in topics)
- `pin`: GPIO pin number (BCM numbering)
- `inverted`: Set `true` if relay is active-low (common for relay modules)
- `ha_name`: Display name in Home Assistant
- `ha_class`: Device class (usually `"switch"`)
- `enabled`: Optional, defaults to `true`

**GPIO Input (Button/Sensor) Settings:**
- `name`: Internal name (used in topics)
- `pin`: GPIO pin number (BCM numbering)
- `pullup`: Enable internal pull-up resistor (typically `true` for buttons)
- `inverted`: Set `true` if button grounds the pin when pressed (typical)
- `linked_output`: Optional - name of output to toggle directly on button press (bypasses MQTT)
- `ha_name`: Display name in Home Assistant
- `ha_class`: Binary sensor class: `door`, `motion`, `occupancy`, `window`, etc. (optional)
- `enabled`: Optional, defaults to `true`

**System Control Settings:**
- `reboot_host.enabled`: Enable/disable reboot button
- `reboot_host.ha_name`: Display name for reboot button
- `reboot_host.icon`: Optional Material Design icon
- `restart_services`: Array of services to create control buttons for
  - `service`: Systemd service name
  - `action`: Action to perform - `"restart"`, `"reload"`, `"start"`, or `"stop"`
  - `ha_name`: Display name in Home Assistant
  - `icon`: Optional Material Design icon
  - `enabled`: Optional, defaults to `true`

**Service Actions:**
- `restart`: Stop and start the service
- `reload`: Reload service configuration without stopping (if supported)
- `start`: Start a stopped service  
- `stop`: Stop a running service

## Advanced Features

### Linked Outputs
Configure `linked_output` on an input to toggle a relay directly via GPIO without MQTT round-trip. Perfect for physical light switches controlling relays - provides instant response independent of network connectivity.

## Home Assistant

After starting the bridge, all entities automatically appear in:
**Settings → Devices & Services → MQTT**

Look for the device: **Wallpanel MQTT Bridge**

### Entity Types

The bridge creates a single unified device with:
- **Switches**: GPIO outputs for relays, LEDs, etc.
- **Binary Sensors**: GPIO inputs for buttons, sensors, etc.
- **Buttons**: IR remote commands
- **Buttons (Diagnostic)**: System controls (reboot, service restart)

## IR Remote Setup

### Hardware Requirements

1. **IR LED** connected to GPIO pin (commonly GPIO 17 or 18)
2. **Enable GPIO IR transmitter** in `/boot/firmware/config.txt`:
   ```
   dtoverlay=gpio-ir-tx,gpio_pin=17
   ```
3. Reboot and verify `/dev/lirc0` exists: `ls -l /dev/lirc0`

### Finding IR Codes

**Method 1: Use LIRC tools to capture codes**
```bash
# Install lirc tools
sudo apt install lirc

# Capture raw IR codes from existing remote
mode2 -d /dev/lirc0

# Press button on remote, you'll see pulse timings
# Example output:
#   space 4434
#   pulse 642
#   space 488
```

**Method 2: Check LIRC remote database**
Many remotes are already documented at [lirc.org/remotes](http://lirc.org/remotes). Download the `.conf` file for your device and extract the timings.

### IR Protocol Configuration

The bridge directly programs the LIRC device using raw pulse timings (LIRC_MODE_PULSE). Key parameters:

- **header**: Initial mark/space pair (usually longest pulses)
- **one/zero**: Mark/space pairs for binary 1 and 0  
- **ptrail**: Final mark pulse
- **gap**: Time between transmissions (affects hold mode speed)
- **frequency**: Carrier frequency (38kHz most common)

**Example: NEC Protocol (most common)**
```json
"header": [9000, 4500],
"one": [560, 1690],
"zero": [560, 560],
"ptrail": 560,
"gap": 100000,
"frequency": 38000
```

## System Controls

### Sudo Permissions

For system controls to work, the service user needs passwordless sudo for specific commands.

**Edit sudoers** (replace `pi` with your username):
```bash
sudo visudo
```

Add these lines:
```
pi ALL=(ALL) NOPASSWD: /usr/bin/systemctl restart wallpanel-mqtt-bridge
pi ALL=(ALL) NOPASSWD: /usr/bin/systemctl reload wallpanel-mqtt-bridge
pi ALL=(ALL) NOPASSWD: /usr/bin/systemctl restart greetd
pi ALL=(ALL) NOPASSWD: /sbin/reboot
```

**Or create a dedicated sudoers file:**
```bash
sudo visudo -f /etc/sudoers.d/wallpanel
```

### Custom Commands

You can define arbitrary shell commands that are exposed as diagnostic buttons in Home Assistant. Commands are executed through `bash -c`, allowing environment variable expansion and pipelines.

**Example uses:**
- Restart compositor: `pkill -u $USER labwc`
- Clear cache: `rm -rf /tmp/cache/*`
- Screenshot: `grim /home/$USER/screenshot.png`
- Custom scripts: `/home/$USER/scripts/toggle-wifi.sh`

**Config example:**
```json
"custom_commands": [
  {
    "name": "restart_labwc",
    "command": "pkill -u $USER labwc",
    "enabled": true,
    "ha_name": "Restart Compositor",
    "icon": "mdi:window-restore"
  },
  {
    "name": "clear_cache",
    "command": "rm -rf /tmp/wallpanel-cache",
    "enabled": true,
    "ha_name": "Clear Cache",
    "icon": "mdi:trash-can"
  }
]
```

**Security considerations:**
- Commands run as the service user (e.g., `pi`)
- No sudo access unless explicitly granted in sudoers
- Use caution with destructive commands
- Consider disabling in production if not needed
- Use absolute paths when possible

### Security Note

System control buttons appear in the "Diagnostic" category in Home Assistant, separate from regular controls. Consider restricting access to these entities in your HA user permissions if sharing dashboard access.

## Troubleshooting

**Check logs:**
```bash
sudo journalctl -u wallpanel-mqtt-bridge -f
```

**Expected log output:**
```
Jan 01 17:17:38 wallpanel wallpanel-mqtt-bridge[12042]: Connected to MQTT
Jan 01 17:17:48 wallpanel wallpanel-mqtt-bridge[12042]: Display: OFF
Jan 01 17:17:53 wallpanel wallpanel-mqtt-bridge[12042]: Sent IR: Brightness Up
Jan 01 17:18:04 wallpanel wallpanel-mqtt-bridge[12042]: Service Restart Kiosk: DONE
```
Status messages (Connected, ON/OFF, IR names, DONE) appear in green for easy monitoring.

**Test MQTT messages:**
```bash
# Listen to all topics
mosquitto_sub -h your-broker -u user -P pass -t '#' -v

# Check discovery
mosquitto_sub -h your-broker -u user -P pass -t 'homeassistant/#' -v

# Check states
mosquitto_sub -h your-broker -u user -P pass -t 'wallpanel/wallpanel-mqtt-bridge/#' -v
```

**IR not working:**
```bash
# Check IR device exists and permissions
ls -l /dev/lirc0
# Should show: crw-rw---- 1 root video

# Verify user is in video group
groups pi
# Should include: video

# Test LIRC device manually
ir-ctl -d /dev/lirc0 --features
# Should show: send receive

# Check dtoverlay is loaded
vcgencmd get_config dtoverlay
# Should include: gpio-ir-tx
```

## Building from Source

```bash
git clone https://github.com/r0bb10/wallpanel-mqtt-bridge
cd wallpanel-mqtt-bridge
go mod download
go build -o wallpanel-mqtt-bridge
```

## License

MIT License - see LICENSE file

## Author

Created by r0bb10