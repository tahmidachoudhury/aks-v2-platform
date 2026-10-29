FROM golang:1.22

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY *.go ./

COPY static ./static

EXPOSE 8086

RUN CGO_ENABLED=0 GOOS=linux go build -o /dashboard-api

CMD ["/dashboard-api"]
