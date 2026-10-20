FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/eze ./cmd/eze
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/eze /usr/local/bin/eze
# A new volume takes this directory's ownership, so the nonroot user can write to it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV EZE_ADDR=:8080 EZE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["eze"]
CMD ["serve"]
