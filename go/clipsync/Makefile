.PHONY: build test

build:
	GOOS=darwin GOARCH=arm64 go build -o clipsync_arm64_macos .
	GOOS=windows GOARCH=amd64 go build -o clipsync_amd64_windows.exe .
	GOOS=linux GOARCH=amd64 go build -o clipsync_amd64_linux .
	cp clipsync_amd64_linux ../../clipsync_amd64_linux.new && mv -f ../../clipsync_amd64_linux.new ../../clipsync_amd64_linux

test:
	go test -v ./...
