#!/bin/bash

# ─────────────────────────────────────────────────────────────────────────────
# Dependency check — must run pre_required_tools.sh first if anything is missing
# ─────────────────────────────────────────────────────────────────────────────
check_prerequisites() {
    local MISSING=()

    if ! command -v pkg-config &>/dev/null; then
        MISSING+=("pkg-config")
    fi

    if ! pkg-config --exists vpx 2>/dev/null; then
        MISSING+=("libvpx")
    fi

    if ! pkg-config --exists opus 2>/dev/null; then
        MISSING+=("libopus / opus")
    fi

    if [[ ${#MISSING[@]} -gt 0 ]]; then
        echo ""
        echo "❌ Missing required libraries: ${MISSING[*]}"
        echo ""
        echo "   Run the installer first:"
        echo "   bash pre_required_tools.sh"
        echo ""
        exit 1
    fi

    echo "✅ All prerequisites satisfied."
}

check_prerequisites

# Default environment
ENV="debug"

# Parse command line arguments
while [[ "$#" -gt 0 ]]; do
    case $1 in
        --env=*) ENV="${1#*=}" ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

# Validate environment
if [[ "$ENV" != "debug" && "$ENV" != "prod" ]]; then
    echo "Invalid environment. Use 'debug' or 'prod'."
    exit 1
fi

# Function to run the Go application
run_app() {
    if [[ "$ENV" == "debug" ]]; then
           echo "Debug mode"
        #    docker-compose up -d 
           nodemon --watch '.' --ext 'go' --exec 'go run main.go --port 8082' --signal SIGTERM --verbose
        # fswatch -o ./**/*.go | xargs -n1 -I{} go run main.go        elif [[ "$ENV" == "prod" ]]; then
            # echo "Production mode not implemented yet."        exit 1
    fi
}

# Run the app initially
run_app

# # Watch for file changes and restart the app
# if command -v inotifywait > /dev/null; then
#     echo "Watching for file changes. Press Ctrl+C to stop."
#     while true; do
#         inotifywait -q -r -e modify,create,delete,move .
#         echo "Changes detected. Restarting the app..."
#         run_app
#     done
# else
#     echo "inotifywait not found. Install it to enable auto-restart on file changes."
#     exit 1
# fi
