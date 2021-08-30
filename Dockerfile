FROM golang:1.16-alpine AS builder
# set arguments user and password
ARG ACCESS_TOKEN_USR="FooBar"
ARG ACCESS_TOKEN_PWD="supersecret"
# install requirement kernel
RUN apk add --no-cache ca-certificates git
# set access owner
RUN mkdir /user && \
    echo 'nobody:x:65534:65534:nobody:/:' > /user/passwd && \
    echo 'nobody:x:65534:' > /user/group
# create netrc for grant access to private repository
RUN printf "machine github.com\n\
    login ${ACCESS_TOKEN_USR}\n\
    password ${ACCESS_TOKEN_PWD}\n"\
    >> /root/.netrc
RUN chmod 600 /root/.netrc
# set working directory
WORKDIR /app
COPY ./go.mod ./go.sum ./
RUN go mod download
# copy all directory
COPY . .
# build the binary
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags '-extldflags "-static"' -o /tmp/app .


FROM alpine:latest AS production
# copy from builder stage to production stage
COPY --from=builder /tmp/app /local/app
COPY ./rtsp-simple-server.yml /local/rtsp-simple-server.yml
# copy directory configuration file to production
# COPY --from=builder /go/src/github.com/gurumedic/gqlapp/files/etc /etc
# set environment before running
ENV APPENV=production

# running the application
CMD ["/local/app", "/local/rtsp-simple-server.yml"]