# Justfile for tulip project

# Default recipe
default: dev

# Build the Go application
build:
    go build -o tulip main.go

# Run the application
run: build
    ./tulip

# Install development tools
install-tools:
    #!/usr/bin/env bash
    set -e
    # Check if air is installed
    if ! command -v air > /dev/null; then
        echo "Installing air for Go hot reloading..."
        go install github.com/air-verse/air@latest
    fi
    
    # Check if browser-sync is installed
    if ! command -v browser-sync > /dev/null; then
        echo "Installing browser-sync for browser reloading..."
        if command -v npm > /dev/null; then
            npm install -g browser-sync
        else
            echo "npm not found. Please install Node.js and npm to use browser-sync."
        fi
    fi

# Run Go server with hot reload
hot-reload:
    #!/usr/bin/env bash
    air & echo $! > .air.pid
    echo "Go server started with hot reloading at http://localhost:8080"

# Run browser sync
browser-sync:
    #!/usr/bin/env bash
    if command -v browser-sync > /dev/null; then
        # Create a directory to monitor for air build completion
        mkdir -p tmp/.air
        
        # Monitor the tmp directory instead of Go files directly
        # This ensures browser only refreshes after successful build
        browser-sync start --proxy "localhost:8080" --files "tmp/**/*" --port 3000 & echo $! > .browsersync.pid
        echo "Browser-sync started at http://localhost:3000"
    else
        echo "Browser-sync not found. Install with npm install -g browser-sync"
    fi

# Clean up processes
cleanup:
    #!/usr/bin/env bash
    # Kill running processes
    if [ -f .air.pid ]; then
        kill $(cat .air.pid) 2>/dev/null || true
        rm .air.pid
    fi
    if [ -f .browsersync.pid ]; then
        kill $(cat .browsersync.pid) 2>/dev/null || true
        rm .browsersync.pid
    fi
    echo "Stopped development processes"

# Clean temporary files
clean: cleanup
    #!/usr/bin/env bash
    rm -rf tmp
    echo "Removed temporary files"

# Run with live reload (main dev command)
dev: install-tools
    #!/usr/bin/env bash
    # Setup trap to clean up child processes
    trap "just cleanup" EXIT INT TERM
    
    # Start processes
    just hot-reload
    sleep 2
    just browser-sync
    
    # Wait for user to press Ctrl+C
    echo "Press Ctrl+C to stop the dev environment"
    tail -f /dev/null