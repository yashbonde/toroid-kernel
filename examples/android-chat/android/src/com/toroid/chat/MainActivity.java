package com.toroid.chat;

import android.Manifest;
import android.app.Activity;
import android.content.ContentResolver;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.database.ContentObserver;
import android.graphics.Color;
import android.graphics.Rect;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.util.Log;
import android.view.View;
import android.view.ViewGroup;
import android.view.ViewTreeObserver;
import android.webkit.CookieManager;
import android.webkit.JavascriptInterface;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import org.json.JSONException;
import org.json.JSONObject;

import java.lang.ref.WeakReference;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.List;

/** A full-screen WebView on the loopback chat server that ChatService runs. */
public class MainActivity extends Activity {
    private static final String BASE = "http://127.0.0.1:" + ChatService.PORT;
    private static volatile WeakReference<MainActivity> current = new WeakReference<>(null);

    /**
     * The global setting that names the phone's shared UI theme: "phosphor"
     * (the default), "garden", "crt" or "custom". Another app (a launcher, say)
     * may write it; this app only reads it. Missing or unknown means phosphor.
     */
    static final String THEME_SETTING = "toroid_ui_theme";
    /**
     * The custom theme's colours: a JSON object from each role in
     * PALETTE_ROLES to "#RRGGBB" (other keys are ignored). With no usable
     * palette, custom is phosphor.
     */
    static final String PALETTE_SETTING = "toroid_ui_palette";
    static final String[] PALETTE_ROLES = {"bg", "surface", "line", "text", "muted", "accent",
            "accent_hi", "warn", "bad", "tint1", "tint2", "tint3"};

    private WebView web;
    private volatile boolean shown; // between onStart and onStop

    // Re-themes the page at once when the theme changes, or the palette does
    // (the page reads it again through ToroidApp.palette()).
    private final ContentObserver themeObserver = new ContentObserver(new Handler(Looper.getMainLooper())) {
        @Override
        public void onChange(boolean selfChange, Uri uri) {
            String t = theme(getContentResolver());
            applyWindowColors(t);
            if (web != null) web.evaluateJavascript("window.applyTheme && window.applyTheme('" + t + "')", null);
        }
    };

    static String theme(ContentResolver cr) {
        String t = Settings.Global.getString(cr, THEME_SETTING);
        if ("custom".equals(t)) return palette(cr) != null ? t : "phosphor";
        return "garden".equals(t) || "crt".equals(t) ? t : "phosphor";
    }

    /** The custom palette, just its roles, or null when it is missing or a role is not "#RRGGBB". */
    static JSONObject palette(ContentResolver cr) {
        String s = Settings.Global.getString(cr, PALETTE_SETTING);
        if (s == null) return null;
        try {
            JSONObject in = new JSONObject(s), out = new JSONObject();
            for (String role : PALETTE_ROLES) {
                String c = in.optString(role);
                if (!c.matches("#[0-9A-Fa-f]{6}")) return null;
                out.put(role, c);
            }
            return out;
        } catch (JSONException e) {
            return null;
        }
    }

    /** The window's bars and the WebView's backdrop, in the page's panel and background colours. */
    private void applyWindowColors(String theme) {
        int panel, bg;
        JSONObject p = "custom".equals(theme) ? palette(getContentResolver()) : null;
        if (p != null) {
            panel = Color.parseColor(p.optString("surface"));
            bg = Color.parseColor(p.optString("bg"));
        } else if ("garden".equals(theme)) {
            panel = Color.rgb(0x1a, 0x1c, 0x20);
            bg = Color.rgb(0x0b, 0x0d, 0x0c);
        } else if ("crt".equals(theme)) {
            panel = Color.rgb(0x0c, 0x11, 0x0e);
            bg = Color.rgb(0x07, 0x0a, 0x08);
        } else {
            panel = Color.rgb(0x16, 0x18, 0x1b);
            bg = Color.rgb(0x0e, 0x0f, 0x11);
        }
        getWindow().setStatusBarColor(panel);
        getWindow().setNavigationBarColor(panel);
        if (web != null) web.setBackgroundColor(bg);
    }

    /** The activity while it is on screen, else null. Android 10 lets an app start activities only then. */
    static MainActivity onScreen() {
        MainActivity a = current.get();
        return a != null && a.shown ? a : null;
    }

    static void finishIfOpen() {
        final MainActivity a = current.get();
        if (a == null) return;
        a.runOnUiThread(new Runnable() {
            @Override
            public void run() {
                a.finishAndRemoveTask();
            }
        });
    }

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        current = new WeakReference<>(this);
        startForegroundService(new Intent(this, ChatService.class));

        web = new WebView(this);
        applyWindowColors(theme(getContentResolver()));
        getContentResolver().registerContentObserver(Settings.Global.getUriFor(THEME_SETTING), false, themeObserver);
        getContentResolver().registerContentObserver(Settings.Global.getUriFor(PALETTE_SETTING), false, themeObserver);
        setContentView(web);
        keepAboveKeyboard();

        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(false);
        web.addJavascriptInterface(new Bridge(), "ToroidApp");
        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                if ("127.0.0.1".equals(u.getHost())) return false;
                // Links in answers open in the browser, not inside the app.
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, u));
                } catch (Exception e) {
                    Log.w(ChatService.TAG, "no app for " + u);
                }
                return true;
            }
        });

        CookieManager cm = CookieManager.getInstance();
        cm.setCookie(BASE, "toroid_token=" + ChatService.token(this) + "; Path=/; HttpOnly; SameSite=Strict");
        cm.flush();

        web.loadData("<html><body style='background:#0e0f11;color:#8b8f96;font-family:sans-serif;"
                + "display:flex;align-items:center;justify-content:center;height:90vh'>Starting…</body></html>",
                "text/html", "utf-8");
        waitForServerThenLoad();
        askForPermissions();
    }

    /**
     * The agent keeps the user's to-dos in the calendar and the documents it
     * saves in Documents/Toroid on shared storage. Both are runtime
     * permissions; without them the tools answer with an error that says how
     * to grant them.
     */
    private void askForPermissions() {
        String[] wanted = {
                Manifest.permission.READ_CALENDAR, Manifest.permission.WRITE_CALENDAR,
                Manifest.permission.READ_EXTERNAL_STORAGE, Manifest.permission.WRITE_EXTERNAL_STORAGE,
        };
        List<String> missing = new ArrayList<>();
        for (String p : wanted) {
            if (checkSelfPermission(p) != PackageManager.PERMISSION_GRANTED) missing.add(p);
        }
        if (!missing.isEmpty()) requestPermissions(missing.toArray(new String[0]), 1);
    }

    @Override
    protected void onStart() {
        super.onStart();
        shown = true;
    }

    @Override
    protected void onStop() {
        shown = false;
        super.onStop();
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        startForegroundService(new Intent(this, ChatService.class));
    }

    /** Polls /healthz until the Go server answers, then shows the chat UI. */
    private void waitForServerThenLoad() {
        new Thread(new Runnable() {
            @Override
            public void run() {
                pollHealth();
            }
        }, "toroidchat-wait").start();
    }

    private void pollHealth() {
        for (int i = 0; i < 100 && !isFinishing(); i++) {
            try {
                HttpURLConnection c = (HttpURLConnection) new URL(BASE + "/healthz").openConnection();
                c.setConnectTimeout(500);
                c.setReadTimeout(500);
                int code = c.getResponseCode();
                c.disconnect();
                if (code == 200) {
                    runOnUiThread(new Runnable() {
                        @Override
                        public void run() {
                            web.loadUrl(BASE + "/");
                        }
                    });
                    return;
                }
            } catch (Exception ignored) {
                // not up yet
            }
            try {
                Thread.sleep(200);
            } catch (InterruptedException e) {
                return;
            }
        }
        runOnUiThread(new Runnable() {
            @Override
            public void run() {
                web.loadData("<html><body style='background:#0e0f11;color:#e58a7c;"
                        + "font-family:sans-serif;padding:24px'>The chat server did not start. "
                        + "See <code>adb logcat -s ToroidChat</code>.</body></html>", "text/html", "utf-8");
            }
        });
    }

    @Override
    public void onBackPressed() {
        if (web != null && web.canGoBack()) {
            web.goBack();
        } else {
            // Keep the chat alive in the background; Stop/Quit ends it.
            moveTaskToBack(true);
        }
    }

    @Override
    protected void onDestroy() {
        getContentResolver().unregisterContentObserver(themeObserver);
        if (current.get() == this) current = new WeakReference<>(null);
        if (web != null) web.destroy();
        super.onDestroy();
    }

    /**
     * Shrinks the WebView to the area above the keyboard. adjustResize alone is
     * not enough: when the status bar is hidden (immersive mode, as with a
     * custom status bar), Android treats the window as full screen and stops
     * resizing it for the keyboard.
     */
    private void keepAboveKeyboard() {
        final View content = findViewById(android.R.id.content);
        content.getViewTreeObserver().addOnGlobalLayoutListener(new ViewTreeObserver.OnGlobalLayoutListener() {
            @Override
            public void onGlobalLayout() {
                Rect visible = new Rect();
                content.getWindowVisibleDisplayFrame(visible);
                int[] at = new int[2];
                content.getLocationOnScreen(at);
                int height = Math.min(visible.bottom - at[1], content.getHeight());
                ViewGroup.LayoutParams lp = web.getLayoutParams();
                if (height > 0 && lp.height != height) {
                    lp.height = height;
                    web.setLayoutParams(lp);
                }
            }
        });
    }

    /** Exposed to the page as window.ToroidApp. */
    private class Bridge {
        /** The shared UI theme: "phosphor", "garden", "crt" or "custom". The page calls this once at load. */
        @JavascriptInterface
        public String theme() {
            return MainActivity.theme(getContentResolver());
        }

        /** The custom theme's palette as JSON, role to "#RRGGBB"; "" when there is none. */
        @JavascriptInterface
        public String palette() {
            JSONObject p = MainActivity.palette(getContentResolver());
            return p != null ? p.toString() : "";
        }

        @JavascriptInterface
        public void quit() {
            runOnUiThread(new Runnable() {
                @Override
                public void run() {
                    stopService(new Intent(MainActivity.this, ChatService.class));
                    finishAndRemoveTask();
                }
            });
        }
    }
}
