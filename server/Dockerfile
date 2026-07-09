FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /claudesession .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S app -G app
# /data is the fly volume mount point for persisted (rotated) credentials.
RUN mkdir /data && chown app:app /data
USER app
WORKDIR /app
COPY --from=build /claudesession ./claudesession
ENV PORT=8080
EXPOSE 8080
CMD ["./claudesession"]
