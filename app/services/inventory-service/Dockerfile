FROM golang:1.22

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download 

COPY *.go ./

EXPOSE 8082

RUN CGO_ENABLED=0 GOOS=linux go build -o /inventory-service

CMD ["/inventory-service"] 
