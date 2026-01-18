rm ./agent_state.json
gofmt -w main.go
go build -o camhub-agent
./camhub-agent
