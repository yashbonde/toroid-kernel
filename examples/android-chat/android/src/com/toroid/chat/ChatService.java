package com.toroid.chat;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.graphics.drawable.Icon;
import android.net.wifi.WifiManager;
import android.os.IBinder;
import android.os.PowerManager;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.security.SecureRandom;
import java.util.Map;

/**
 * Foreground service that runs the Go chat server (libtoroidchat.so) as a
 * child process and keeps the phone able to reach the network while it runs:
 * a partial wake lock keeps the CPU up and a high-performance Wi-Fi lock stops
 * the radio from dropping when the screen sleeps. Everything is released when
 * the user taps Stop on the notification or Quit in the app.
 *
 * <p>It also runs the {@link ToolHost}, through which the server's agent reads
 * and writes the to-do list in the phone's calendar.
 */
public class ChatService extends Service {
    static final String TAG = "ToroidChat";
    static final int PORT = 18765;
    static final String ACTION_STOP = "com.toroid.chat.STOP";
    private static final String CHANNEL = "server";
    private static final int NOTIFICATION_ID = 1;

    private static String token;

    /**
     * The server's secret, which the WebView presents as a cookie (other apps
     * can reach loopback too). It is kept in files/token (mode 600; the server
     * writes it there) and reused across restarts, so a client given a copy,
     * such as the Gallery app, keeps working after the app or phone restarts.
     * A missing or malformed file gets a fresh random token.
     */
    static synchronized String token(Context ctx) {
        if (token == null) {
            token = readToken(new File(ctx.getFilesDir(), "token"));
        }
        if (token == null) {
            byte[] b = new byte[16];
            new SecureRandom().nextBytes(b);
            StringBuilder sb = new StringBuilder();
            for (byte x : b) sb.append(String.format("%02x", x & 0xff));
            token = sb.toString();
        }
        return token;
    }

    private static String readToken(File f) {
        try {
            byte[] b = new byte[64];
            FileInputStream in = new FileInputStream(f);
            int n;
            try {
                n = in.read(b);
            } finally {
                in.close();
            }
            String t = n > 0 ? new String(b, 0, n, "US-ASCII").trim() : "";
            return t.matches("[0-9a-f]{32}") ? t : null;
        } catch (IOException e) {
            return null;
        }
    }

    private PowerManager.WakeLock wakeLock;
    private WifiManager.WifiLock wifiLock;
    private volatile boolean stopping;
    private volatile Process proc;
    private OutputStream procStdin; // held open: the server exits when it closes
    private Thread supervisor;
    private ToolHost toolHost;

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = getSystemService(NotificationManager.class);
        NotificationChannel ch = new NotificationChannel(CHANNEL, "Chat server", NotificationManager.IMPORTANCE_LOW);
        ch.setDescription("Shown while the chat server is running");
        nm.createNotificationChannel(ch);
        startForeground(NOTIFICATION_ID, buildNotification());

        PowerManager pm = getSystemService(PowerManager.class);
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "toroidchat:server");
        wakeLock.setReferenceCounted(false);
        wakeLock.acquire();
        WifiManager wm = (WifiManager) getApplicationContext().getSystemService(WIFI_SERVICE);
        if (wm != null) {
            wifiLock = wm.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "toroidchat:server");
            wifiLock.setReferenceCounted(false);
            wifiLock.acquire();
        }
        Log.i(TAG, "service started; wake lock and wifi lock held");

        // Up before the server, so its first kernel can count the open to-dos.
        toolHost = new ToolHost(this);
        toolHost.start();

        // Anonymous classes, not lambdas: android.jar has no LambdaMetafactory
        // for javac to link against in this Gradle-less build.
        supervisor = new Thread(new Runnable() {
            @Override
            public void run() {
                superviseServer();
            }
        }, "toroidchat-supervisor");
        supervisor.start();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {
            Log.i(TAG, "stop requested from notification");
            MainActivity.finishIfOpen();
            stopSelf();
        }
        // Not sticky: once stopped, nothing should bring the wake locks back
        // until the user opens the app again.
        return START_NOT_STICKY;
    }

    private Notification buildNotification() {
        PendingIntent open = PendingIntent.getActivity(this, 0,
                new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
        PendingIntent stop = PendingIntent.getService(this, 1,
                new Intent(this, ChatService.class).setAction(ACTION_STOP), PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle("Toroid Chat is running")
                .setContentText("Keeping the network awake for replies. Stop to save battery.")
                .setContentIntent(open)
                .setOngoing(true)
                .addAction(new Notification.Action.Builder(
                        Icon.createWithResource(this, R.drawable.ic_stat), "Stop", stop).build())
                .build();
    }

    /** Runs the server, restarting it with backoff if it dies on its own. */
    private void superviseServer() {
        long backoff = 1000;
        while (!stopping) {
            long started = System.currentTimeMillis();
            int code = runServerOnce();
            if (stopping) break;
            Log.w(TAG, "server exited with " + code + "; restarting in " + backoff + "ms");
            try {
                Thread.sleep(backoff);
            } catch (InterruptedException e) {
                break;
            }
            backoff = System.currentTimeMillis() - started > 60000 ? 1000 : Math.min(backoff * 2, 30000);
        }
    }

    private int runServerOnce() {
        File bin = new File(getApplicationInfo().nativeLibraryDir, "libtoroidchat.so");
        File files = getFilesDir();
        ProcessBuilder pb = new ProcessBuilder(bin.getAbsolutePath(),
                "-addr", "127.0.0.1:" + PORT,
                "-data", files.getAbsolutePath(),
                "-watch-stdin");
        Map<String, String> env = pb.environment();
        env.put("HOME", files.getAbsolutePath());
        env.put("TOROID_HOME", new File(files, "toroid").getAbsolutePath());
        env.put("TMPDIR", getCacheDir().getAbsolutePath());
        env.put("TOROID_CHAT_TOKEN", token(this));
        // Go itself starts in UTC on Android. The kernel falls back to
        // `getprop persist.sys.timezone`; passing TZ skips that process spawn.
        env.put("TZ", java.util.TimeZone.getDefault().getID());
        pb.directory(files);
        pb.redirectErrorStream(true);
        try {
            Process p = pb.start();
            proc = p;
            procStdin = p.getOutputStream();
            BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream()));
            String line;
            while ((line = r.readLine()) != null) {
                Log.i(TAG, "server: " + line);
            }
            return p.waitFor();
        } catch (Exception e) {
            // onDestroy closing the pipes is the normal way out; only a failure
            // while still meant to be running is worth a stack trace.
            if (!stopping) Log.e(TAG, "server failed to run", e);
            return -1;
        }
    }

    @Override
    public void onDestroy() {
        stopping = true;
        Process p = proc;
        if (p != null) {
            p.destroy();
            try {
                if (!p.waitFor(2, java.util.concurrent.TimeUnit.SECONDS)) p.destroyForcibly();
            } catch (InterruptedException ignored) {
                p.destroyForcibly();
            }
        }
        if (supervisor != null) supervisor.interrupt();
        if (toolHost != null) toolHost.stop();
        if (wifiLock != null && wifiLock.isHeld()) wifiLock.release();
        if (wakeLock != null && wakeLock.isHeld()) wakeLock.release();
        Log.i(TAG, "service stopped; locks released");
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
