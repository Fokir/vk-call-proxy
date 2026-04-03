#!/bin/sh
# Start virtual display (1920x1080, 24-bit color)
Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp &
export DISPLAY=:99

# Wait for Xvfb to start
sleep 1

# Run captcha-service (Chrome will use the virtual display)
exec captcha-service "$@"
