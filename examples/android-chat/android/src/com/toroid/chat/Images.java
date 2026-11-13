package com.toroid.chat;

import android.Manifest;
import android.content.Context;
import android.content.pm.PackageManager;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.media.MediaScannerConnection;
import android.net.Uri;
import android.os.Environment;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.security.SecureRandom;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.regex.Pattern;

/**
 * The image agent's shared-storage I/O (images.go), so the Go server never
 * touches shared storage itself:
 *
 * <ul>
 * <li>image_publish {src, name}: copies an image the server wrote into its own
 * files/images dir to Pictures/Toroid/name, tells the media scanner (so the
 * Gallery shows it at once) and returns its path and content URI.
 * <li>image_load {path, max}: decodes an image from shared storage, scaled so
 * its longer side is at most max pixels, into a private PNG in the cache dir
 * and returns that file's path. The server reads it and deletes it.
 * </ul>
 */
final class Images {
    static final String SHOWN_AS = "Pictures/Toroid";
    private static final Pattern NAME = Pattern.compile("toroid-[0-9]{8}-[0-9]{6}-[0-9a-f]{4}\\.(png|jpg|webp)");
    private static final int MAX_SIDE = 2048;

    private final Context ctx;

    Images(Context ctx) {
        this.ctx = ctx.getApplicationContext();
    }

    Object run(String op, JSONObject a) throws Todos.Failure, JSONException {
        if (ctx.checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) != PackageManager.PERMISSION_GRANTED
                || ctx.checkSelfPermission(Manifest.permission.READ_EXTERNAL_STORAGE) != PackageManager.PERMISSION_GRANTED) {
            throw new Todos.Failure("Toroid Chat has no storage permission, so it cannot reach " + SHOWN_AS + ". "
                    + "Open the app and allow Storage access, or turn it on in Settings > Apps > Toroid Chat > Permissions.");
        }
        if (!Environment.MEDIA_MOUNTED.equals(Environment.getExternalStorageState())) {
            throw new Todos.Failure("the phone's shared storage is not available right now");
        }
        switch (op) {
            case "image_publish":
                return publish(a);
            case "image_load":
                return load(a);
            default:
                throw new Todos.Failure("unknown op: " + op);
        }
    }

    /** Pictures/Toroid on shared storage. */
    @SuppressWarnings("deprecation") // legacy storage: the app opts in with requestLegacyExternalStorage
    static File dir() {
        return new File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_PICTURES), "Toroid");
    }

    private JSONObject publish(JSONObject a) throws Todos.Failure, JSONException {
        String name = a.optString("name", "");
        if (!NAME.matcher(name).matches()) throw new Todos.Failure("bad image name \"" + name + "\"");
        File src;
        try {
            File own = new File(ctx.getFilesDir(), "images").getCanonicalFile();
            src = new File(a.optString("src", "")).getCanonicalFile();
            if (!own.equals(src.getParentFile()) || !src.isFile()) throw new Todos.Failure("bad image source");
        } catch (IOException e) {
            throw new Todos.Failure("bad image source: " + e.getMessage());
        }
        File d = dir();
        if (!d.isDirectory() && !d.mkdirs()) throw new Todos.Failure("could not create " + SHOWN_AS);
        File f = new File(d, name);
        if (f.exists()) throw new Todos.Failure(name + " already exists in " + SHOWN_AS);
        File tmp = new File(d, "." + name + ".tmp");
        try {
            copy(src, tmp);
            if (!tmp.renameTo(f)) throw new IOException("rename failed");
        } catch (IOException e) {
            //noinspection ResultOfMethodCallIgnored
            tmp.delete();
            throw new Todos.Failure("could not save " + name + ": " + e.getMessage());
        }
        Uri uri = scan(f);
        JSONObject o = new JSONObject();
        o.put("path", f.getAbsolutePath());
        o.put("shown_as", SHOWN_AS + "/" + name);
        if (uri != null) o.put("uri", uri.toString());
        return o;
    }

    @SuppressWarnings("deprecation") // legacy storage, as dir()
    private JSONObject load(JSONObject a) throws Todos.Failure, JSONException {
        int max = Math.max(16, Math.min(MAX_SIDE, a.optInt("max", 1024)));
        File f;
        try {
            File root = Environment.getExternalStorageDirectory().getCanonicalFile();
            f = new File(a.optString("path", "")).getCanonicalFile();
            if (!f.getPath().startsWith(root.getPath() + File.separator)) {
                throw new Todos.Failure("only images in the phone's shared storage can be read");
            }
        } catch (IOException e) {
            throw new Todos.Failure("bad image path: " + e.getMessage());
        }
        if (!f.isFile()) throw new Todos.Failure("no image at " + f.getPath());
        BitmapFactory.Options bounds = new BitmapFactory.Options();
        bounds.inJustDecodeBounds = true;
        BitmapFactory.decodeFile(f.getPath(), bounds);
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) throw new Todos.Failure(f.getName() + " is not an image this phone can read");
        BitmapFactory.Options opts = new BitmapFactory.Options();
        opts.inSampleSize = 1; // 0 by default
        int longest = Math.max(bounds.outWidth, bounds.outHeight);
        while (longest / (opts.inSampleSize * 2) >= max) opts.inSampleSize *= 2;
        Bitmap bm = BitmapFactory.decodeFile(f.getPath(), opts);
        if (bm == null) throw new Todos.Failure("could not decode " + f.getName());
        float scale = Math.min(1f, (float) max / Math.max(bm.getWidth(), bm.getHeight()));
        if (scale < 1f) {
            Bitmap s = Bitmap.createScaledBitmap(bm, Math.max(1, Math.round(bm.getWidth() * scale)),
                    Math.max(1, Math.round(bm.getHeight() * scale)), true);
            if (s != bm) bm.recycle();
            bm = s;
        }
        byte[] r = new byte[6];
        new SecureRandom().nextBytes(r);
        StringBuilder sb = new StringBuilder("load-");
        for (byte x : r) sb.append(String.format("%02x", x & 0xff));
        File out = new File(ctx.getCacheDir(), sb.append(".png").toString());
        try {
            OutputStream os = new FileOutputStream(out);
            try {
                bm.compress(Bitmap.CompressFormat.PNG, 100, os);
            } finally {
                os.close();
            }
        } catch (IOException e) {
            //noinspection ResultOfMethodCallIgnored
            out.delete();
            throw new Todos.Failure("could not read " + f.getName() + ": " + e.getMessage());
        }
        JSONObject o = new JSONObject();
        o.put("src", out.getAbsolutePath());
        o.put("width", bm.getWidth());
        o.put("height", bm.getHeight());
        o.put("orig_width", bounds.outWidth);
        o.put("orig_height", bounds.outHeight);
        bm.recycle();
        return o;
    }

    private static void copy(File from, File to) throws IOException {
        InputStream in = new FileInputStream(from);
        try {
            FileOutputStream out = new FileOutputStream(to);
            try {
                byte[] buf = new byte[64 * 1024];
                int n;
                while ((n = in.read(buf)) > 0) out.write(buf, 0, n);
                out.getFD().sync();
            } finally {
                out.close();
            }
        } finally {
            in.close();
        }
    }

    /** Tells the media scanner and waits briefly for the image's content URI. */
    private Uri scan(File f) {
        final Uri[] got = new Uri[1];
        final CountDownLatch done = new CountDownLatch(1);
        MediaScannerConnection.scanFile(ctx, new String[]{f.getAbsolutePath()}, null,
                new MediaScannerConnection.OnScanCompletedListener() {
                    @Override
                    public void onScanCompleted(String path, Uri uri) {
                        got[0] = uri;
                        done.countDown();
                    }
                });
        try {
            done.await(5, TimeUnit.SECONDS);
        } catch (InterruptedException ignored) {
            // answer without the URI
        }
        return got[0];
    }
}
