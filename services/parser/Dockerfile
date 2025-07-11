FROM golang:1.24 as build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /parser ./cmd

FROM gcr.io/distroless/base-debian12
COPY --from=build /parser /parser
ENTRYPOINT ["/parser"]
