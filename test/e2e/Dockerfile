FROM alpine:3.20
COPY anymcp /usr/local/bin/anymcp
RUN chmod 0755 /usr/local/bin/anymcp
COPY gateway.json /etc/anymcp/
ENTRYPOINT ["anymcp"]
