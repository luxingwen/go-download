export GOPROXY=https://goproxy.io
default: build

build: export GO111MODULE=on

build:
	go build -o go-download-linux main.go
	GOOS=darwin go build -o go-download-mac main.go


	