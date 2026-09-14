FROM alpine:3.20
COPY vecta /usr/local/bin/vecta
RUN chmod 0755 /usr/local/bin/vecta
COPY gateway.json /etc/vecta/
ENTRYPOINT ["vecta"]
