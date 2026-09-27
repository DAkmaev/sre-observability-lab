FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o /out/app .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/app /usr/local/bin/app
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/app"]
