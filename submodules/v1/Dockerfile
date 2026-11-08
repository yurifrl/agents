# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agents ./cmd/agents

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agents /usr/local/bin/agents
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/agents"]
