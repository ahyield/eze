FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/eze ./cmd/eze

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/eze /usr/local/bin/eze
ENV EZE_ADDR=:8080 EZE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["eze"]
CMD ["serve"]
