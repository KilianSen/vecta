package group.senger.anymcp.universal;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;

/** The gateway owner API (docs/owner-api.md). */
final class GatewayClient {
    private final String base;
    private final String token;

    GatewayClient(String base, String token) {
        this.base = base;
        this.token = token;
    }

    void register(String id, String json) throws IOException {
        request("PUT", serverUrl(id), json, 10000);
    }

    void unregister(String id) throws IOException {
        request("DELETE", serverUrl(id), null, 10000);
    }

    /** Requests a transfer ticket for a player (docs/owner-api.md). */
    Map<String, Object> ticket(String serverId, String player, int protocol, String target) throws IOException {
        Map<String, Object> req = new LinkedHashMap<String, Object>();
        req.put("player", player);
        req.put("protocol", protocol);
        req.put("target", target);
        String resp = request("POST", serverUrl(serverId) + "/transfer-ticket", Json.write(req), 10000);
        return Json.obj(Json.parse(resp));
    }

    void postGlobal(String serverId, String player, String message) throws IOException {
        Map<String, Object> req = new LinkedHashMap<String, Object>();
        req.put("player", player);
        req.put("message", message);
        request("POST", serverUrl(serverId) + "/global", Json.write(req), 10000);
    }

    /** Returns the current global seq without waiting, to initialize a poller. */
    Map<String, Object> pollGlobalInit() throws IOException {
        return Json.obj(Json.parse(request("GET", base + "/api/v1/global", null, 10000)));
    }

    /** Long-polls for global messages newer than after; returns {messages, seq}. */
    Map<String, Object> pollGlobal(long after) throws IOException {
        String resp = request("GET", base + "/api/v1/global?after=" + after, null, 35000);
        return Json.obj(Json.parse(resp));
    }

    private String serverUrl(String id) throws IOException {
        return base + "/api/v1/servers/" + URLEncoder.encode(id, "UTF-8");
    }

    private String request(String method, String url, String body, int readTimeoutMs) throws IOException {
        HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
        c.setConnectTimeout(5000);
        c.setReadTimeout(readTimeoutMs);
        c.setRequestMethod(method);
        c.setRequestProperty("Authorization", "Bearer " + token);
        c.setRequestProperty("User-Agent", "anymcp-jar/" + Anymcp.version());
        if (body != null) {
            byte[] b = body.getBytes(StandardCharsets.UTF_8);
            c.setDoOutput(true);
            c.setRequestProperty("Content-Type", "application/json");
            c.setFixedLengthStreamingMode(b.length);
            OutputStream out = c.getOutputStream();
            try {
                out.write(b);
            } finally {
                out.close();
            }
        }
        int code = c.getResponseCode();
        InputStream in = code >= 400 ? c.getErrorStream() : c.getInputStream();
        String resp = "";
        if (in != null) {
            try {
                resp = new String(ModScanner.readAll(in, 1 << 20), StandardCharsets.UTF_8);
            } finally {
                in.close();
            }
        }
        if (code >= 300) {
            throw new IOException(method + " " + url + ": HTTP " + code + " " + resp.trim());
        }
        return resp;
    }
}
