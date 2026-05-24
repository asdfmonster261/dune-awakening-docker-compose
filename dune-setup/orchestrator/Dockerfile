FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/orchestrator ./...

FROM scratch
COPY --from=build /out/orchestrator /orchestrator
EXPOSE 6443
ENTRYPOINT ["/orchestrator"]
