# Build the manager binary
FROM golang:1.15.3 as builder

WORKDIR /app

# cache deps before building and copying source so that we don't need to   re-download as much
# and so that source changes don't invalidate our downloaded layer

# In case of network error while building images manually
ENV GOPROXY=https://goproxy.io

# Copy the go source
COPY ./ .

# Build
RUN CGO_ENABLED=0 GOOS=linux GO111MODULE=on go build -o hyperfed cmd/hyperfed/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more     details
FROM alpine:latest

WORKDIR /hyperfed

RUN apk --no-cache add ca-certificates
RUN adduser -D -g hyperfed -u 1001 hyperfed

COPY --from=builder /app/hyperfed .

RUN ln -s hyperfed controller-manager \
 && ln -s hyperfed kubefedctl \
 && ln -s hyperfed webhook

RUN chown -R hyperfed:hyperfed /hyperfed

USER hyperfed
ENTRYPOINT ["./controller-manager"]
