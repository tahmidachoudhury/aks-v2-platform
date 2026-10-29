FROM golang:1.22@sha256:1cf6c45ba39db9fd6db16922041d074a63c935556a05c5ccb62d181034df7f02 AS build

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY *.go ./

COPY static ./static

RUN CGO_ENABLED=0 GOOS=linux go build -o /dashboard-api

FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2

COPY --from=build /dashboard-api /dashboard-api

EXPOSE 8086

CMD ["/dashboard-api"]
