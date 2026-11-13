package com.toroid.chat;

import android.content.Context;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import android.os.Process;
import android.util.Log;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStreamWriter;
import java.io.Writer;
import java.nio.charset.StandardCharsets;

/**
 * The phone side of the agent's tools. The Go server cannot use Android APIs,
 * so this listens on an abstract-namespace Unix socket and runs the to-do ops
 * ({@link Todos}), the file ops ({@link Files}, every op starting "file") and
 * the image ops ({@link Images}, every op starting "image") for it.
 *
 * <p>Protocol: one JSON request per line, {"op": "...", "args": {...}}, answered
 * by one JSON line, {"ok": true, "result": ...} or {"ok": false, "error": "..."}.
 * Each connection is served on its own thread.
 *
 * <p>Any app can connect to an abstract socket, so a peer is served only when
 * its uid is this app's: the Go server runs as our child process, under our uid.
 */
final class ToolHost {
    static final String SOCKET = "toroidchat-tools";
    private static final int READ_TIMEOUT_MS = 30000;

    private final Todos todos;
    private final Files files;
    private final Images images;
    private volatile boolean stopping;
    private LocalServerSocket server;

    ToolHost(Context ctx) {
        todos = new Todos(ctx);
        files = new Files(ctx);
        images = new Images(ctx);
    }

    void start() {
        try {
            server = new LocalServerSocket(SOCKET);
        } catch (IOException e) {
            Log.e(ChatService.TAG, "tool host: cannot listen on @" + SOCKET, e);
            return;
        }
        new Thread(new Runnable() {
            @Override
            public void run() {
                acceptLoop();
            }
        }, "toroidchat-tools").start();
        Log.i(ChatService.TAG, "tool host listening on @" + SOCKET);
    }

    void stop() {
        stopping = true;
        LocalServerSocket s = server;
        if (s == null) return;
        // close() does not wake a thread blocked in accept(); a connection does.
        LocalSocket wake = new LocalSocket();
        try {
            wake.connect(new LocalSocketAddress(SOCKET));
        } catch (IOException ignored) {
            // nothing is listening any more
        } finally {
            closeQuietly(wake);
        }
        try {
            s.close();
        } catch (IOException ignored) {
            // already closed
        }
    }

    private void acceptLoop() {
        final int uid = Process.myUid();
        while (!stopping) {
            final LocalSocket peer;
            try {
                peer = server.accept();
            } catch (IOException e) {
                if (!stopping) Log.w(ChatService.TAG, "tool host: accept failed", e);
                return;
            }
            if (stopping) {
                closeQuietly(peer);
                return;
            }
            int peerUid;
            try {
                peerUid = peer.getPeerCredentials().getUid();
            } catch (IOException e) {
                closeQuietly(peer);
                continue;
            }
            if (peerUid != uid) {
                Log.w(ChatService.TAG, "tool host: refused a connection from uid " + peerUid);
                closeQuietly(peer);
                continue;
            }
            new Thread(new Runnable() {
                @Override
                public void run() {
                    serve(peer);
                }
            }, "toroidchat-tool").start();
        }
    }

    private void serve(LocalSocket peer) {
        try {
            peer.setSoTimeout(READ_TIMEOUT_MS);
            BufferedReader in = new BufferedReader(new InputStreamReader(peer.getInputStream(), StandardCharsets.UTF_8));
            Writer out = new OutputStreamWriter(peer.getOutputStream(), StandardCharsets.UTF_8);
            String line;
            while ((line = in.readLine()) != null) {
                if (line.trim().isEmpty()) continue;
                out.write(handle(line));
                out.write('\n');
                out.flush();
            }
        } catch (IOException e) {
            // the peer went away or timed out; nothing to answer
        } finally {
            closeQuietly(peer);
        }
    }

    /** Runs one request line and returns the response line. */
    private String handle(String line) {
        JSONObject resp = new JSONObject();
        String op = "?";
        try {
            JSONObject req = new JSONObject(line);
            op = req.getString("op");
            JSONObject args = req.optJSONObject("args");
            JSONObject a = args != null ? args : new JSONObject();
            Object result = op.startsWith("file") ? files.run(op, a)
                    : op.startsWith("image") ? images.run(op, a)
                    : todos.run(op, a);
            resp.put("ok", true);
            resp.put("result", result);
        } catch (Todos.Failure e) {
            fail(resp, e.getMessage());
        } catch (JSONException e) {
            fail(resp, "bad request: " + e.getMessage());
        } catch (RuntimeException e) {
            // A provider error (a SecurityException after a permission was
            // revoked, say) must not kill the thread without an answer.
            Log.w(ChatService.TAG, "tool host: " + op + " failed", e);
            fail(resp, op + " failed: " + e.getMessage());
        }
        return resp.toString();
    }

    private static void fail(JSONObject resp, String msg) {
        try {
            resp.put("ok", false);
            resp.put("error", msg);
        } catch (JSONException ignored) {
            // put only throws for non-finite numbers
        }
    }

    private static void closeQuietly(LocalSocket s) {
        try {
            s.close();
        } catch (IOException ignored) {
            // already closed
        }
    }
}
