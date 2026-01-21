#!/bin/bash
set -e

# Wallpanel MQTT Bridge Installation Script
# Usage: sudo ./install.sh

if [ "$EUID" -ne 0 ]; then 
    echo "Please run as root (use sudo)"
    exit 1
fi

# Get the user who invoked sudo
ACTUAL_USER="${SUDO_USER:-$USER}"
if [ "$ACTUAL_USER" = "root" ]; then
    echo "Error: Please run this script with sudo, not as root directly"
    echo "Usage: sudo ./install.sh"
    exit 1
fi

INSTALL_DIR="/opt/wallpanel-mqtt-bridge"
SERVICE_NAME="wallpanel-mqtt-bridge"

echo "Installing Wallpanel MQTT Bridge for user: $ACTUAL_USER"

# Create installation directory
echo "Creating installation directory: $INSTALL_DIR"
mkdir -p $INSTALL_DIR

# Copy files
echo "Installing files..."
cp wallpanel-mqtt-bridge $INSTALL_DIR/
chmod +x $INSTALL_DIR/wallpanel-mqtt-bridge

# Copy or create config if it doesn't exist
if [ ! -f "$INSTALL_DIR/config.json" ]; then
    echo "Installing default config..."
    cp config.json $INSTALL_DIR/
else
    echo "Config already exists, keeping existing config"
    echo "New default config saved as: $INSTALL_DIR/config.json.example"
    cp config.json $INSTALL_DIR/config.json.example
fi

# Set ownership to the actual user
chown -R $ACTUAL_USER:$ACTUAL_USER $INSTALL_DIR

# Install systemd service with the correct user
echo "Installing systemd service for user: $ACTUAL_USER"
sed "s/User=.*/User=$ACTUAL_USER/" wallpanel-mqtt-bridge.service | \
sed "s/Group=.*/Group=$ACTUAL_USER/" > /etc/systemd/system/$SERVICE_NAME.service

systemctl daemon-reload

echo ""
echo "Installation complete!"
echo ""
echo "Next steps:"
echo "1. Edit the config: sudo nano $INSTALL_DIR/config.json"
echo "2. Enable service: sudo systemctl enable $SERVICE_NAME"
echo "3. Start service: sudo systemctl start $SERVICE_NAME"
echo "4. Check status: sudo systemctl status $SERVICE_NAME"
echo "5. View logs: sudo journalctl -u $SERVICE_NAME -f"
echo ""