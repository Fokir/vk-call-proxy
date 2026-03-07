# CallVPN iOS Build
#
# Prerequisites:
#   - Go 1.21+ with gomobile: go install golang.org/x/mobile/cmd/gomobile@latest
#   - gomobile init
#   - xcodegen: brew install xcodegen
#   - Xcode 15+
#
# Usage:
#   make framework   — build Bind.xcframework from Go code
#   make project     — generate Xcode project from project.yml
#   make build       — build framework + generate project
#   make clean       — remove generated artifacts

GOMOBILE ?= $(shell go env GOPATH)/bin/gomobile
BIND_PKG = ../../mobile/bind
FRAMEWORK = Bind.xcframework

.PHONY: framework project build clean

# Build Go bindings as xcframework
framework:
	$(GOMOBILE) bind -target=ios -o $(FRAMEWORK) $(BIND_PKG)

# Generate Xcode project from project.yml (requires xcodegen)
project:
	xcodegen generate

# Full build: framework + Xcode project
build: framework project

clean:
	rm -rf $(FRAMEWORK)
	rm -rf CallVPN.xcodeproj
