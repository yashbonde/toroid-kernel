package com.toroid.chat;

import android.Manifest;
import android.content.ActivityNotFoundException;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.media.MediaScannerConnection;
import android.net.Uri;
import android.os.Environment;
import android.webkit.MimeTypeMap;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.text.SimpleDateFormat;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.Comparator;
import java.util.Date;
import java.util.HashSet;
import java.util.List;
import java.util.Locale;
import java.util.Set;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.regex.Pattern;

/**
 * The agent's documents, kept in shared storage at Documents/Toroid so they
 * show in the phone's Files app (Documents › Toroid).
 *
 * <p>Every op names one plain file in that directory. A name is checked
 * against {@link #NAME} (no folders, no "..", nothing hidden) and then
 * resolved to its canonical path, which must sit directly in the directory.
 * Saves are text only, at most 1 MB, and never replace a file unless asked.
 * After a write or delete the media scanner is told, so Files, Gallery and
 * Music see the change at once.
 *
 * <p>file_open shows a file through {@link FilesProvider}: HTML as text/html
 * (HTMLViewer), media under its own MIME type (Gallery, Music), any other text
 * as text/plain.
 */
final class Files {
    static final String AUTHORITY = "com.toroid.chat.files";
    static final String SHOWN_AS = "Documents/Toroid"; // as the Files app shows it
    private static final int MAX_SAVE = 1 << 20;
    private static final int MAX_READ = 64 * 1024;
    private static final Pattern NAME = Pattern.compile("[A-Za-z0-9_()-][A-Za-z0-9 ._()-]{0,79}\\.[A-Za-z0-9]{1,10}");
    /** Extensions file_save writes, and file_open shows as text. Kept in step with files.go. */
    private static final Set<String> TEXT = new HashSet<>(Arrays.asList(
            "html", "htm", "txt", "md", "markdown", "json", "csv", "tsv", "xml", "yaml", "yml",
            "toml", "ini", "log", "css", "js", "ts", "py", "go", "java", "kt", "c", "h",
            "cpp", "rs", "rb", "swift", "sh", "sql"));

    private final Context ctx;

    Files(Context ctx) {
        this.ctx = ctx.getApplicationContext();
    }

    /** Runs one op; the result is a JSONObject or JSONArray. */
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
            case "files_list":
                return list();
            case "file_save":
                return save(a);
            case "file_read":
                return read(a);
            case "file_open":
                return open(a);
            case "file_delete":
                return delete(a);
            default:
                throw new Todos.Failure("unknown op: " + op);
        }
    }

    // ---- ops ----------------------------------------------------------------

    private JSONArray list() throws JSONException {
        File[] all = dir().listFiles();
        List<File> files = new ArrayList<>();
        if (all != null) {
            for (File f : all) {
                if (f.isFile() && !f.getName().startsWith(".")) files.add(f);
            }
        }
        Collections.sort(files, new Comparator<File>() {
            @Override
            public int compare(File x, File y) {
                int d = Long.compare(y.lastModified(), x.lastModified()); // newest first
                return d != 0 ? d : x.getName().compareTo(y.getName());
            }
        });
        SimpleDateFormat fmt = new SimpleDateFormat("yyyy-MM-dd HH:mm", Locale.ROOT);
        JSONArray out = new JSONArray();
        for (File f : files) {
            JSONObject o = new JSONObject();
            o.put("name", f.getName());
            o.put("size", f.length());
            o.put("modified", fmt.format(new Date(f.lastModified())));
            o.put("type", mimeOf(f.getName()));
            out.put(o);
        }
        return out;
    }

    private synchronized JSONObject save(JSONObject a) throws Todos.Failure, JSONException {
        String name = a.optString("name", "");
        File f = resolve(name);
        String ext = ext(f.getName());
        if (!TEXT.contains(ext)) {
            throw new Todos.Failure("cannot save " + f.getName() + ": file_save writes text, and ." + ext + " is not a text format");
        }
        byte[] data = a.optString("content", "").getBytes(StandardCharsets.UTF_8);
        if (data.length == 0) throw new Todos.Failure("content is empty");
        if (data.length > MAX_SAVE) throw new Todos.Failure("content is " + data.length + " bytes; a saved file can be at most 1 MB");
        File d = f.getParentFile();
        if (!d.isDirectory() && !d.mkdirs()) throw new Todos.Failure("could not create " + SHOWN_AS);
        if (f.isDirectory()) throw new Todos.Failure(f.getName() + " is a folder");
        if (f.exists() && !a.optBoolean("overwrite", false)) {
            throw new Todos.Failure(f.getName() + " already exists in " + SHOWN_AS
                    + "; choose another name, or pass overwrite if the user asked to replace it");
        }
        // Write a hidden temp file, then rename it over the name, so a reader
        // never sees half a file.
        File tmp = new File(d, "." + f.getName() + ".tmp");
        try {
            FileOutputStream out = new FileOutputStream(tmp);
            try {
                out.write(data);
                out.getFD().sync();
            } finally {
                out.close();
            }
            if (!tmp.renameTo(f)) throw new IOException("rename failed");
        } catch (IOException e) {
            //noinspection ResultOfMethodCallIgnored
            tmp.delete();
            throw new Todos.Failure("could not save " + f.getName() + ": " + e.getMessage());
        }
        scan(f);
        JSONObject o = new JSONObject();
        o.put("name", f.getName());
        o.put("path", SHOWN_AS + "/" + f.getName());
        o.put("bytes", data.length);
        return o;
    }

    private JSONObject read(JSONObject a) throws Todos.Failure, JSONException {
        File f = existing(a.optString("name", ""));
        String name = f.getName();
        String kind = kindOf(name);
        if (!kind.equals("html") && !kind.equals("text")) {
            throw new Todos.Failure(name + " is " + describe(name, f.length()) + ", so it cannot be read as text. "
                    + "Use file_open to show it to the user.");
        }
        long size = f.length();
        byte[] buf = new byte[(int) Math.min(size, MAX_READ)];
        int n = 0;
        try {
            InputStream in = new FileInputStream(f);
            try {
                int r;
                while (n < buf.length && (r = in.read(buf, n, buf.length - n)) > 0) n += r;
            } finally {
                in.close();
            }
        } catch (IOException e) {
            throw new Todos.Failure("could not read " + name + ": " + e.getMessage());
        }
        for (int i = 0; i < n; i++) {
            if (buf[i] == 0) throw new Todos.Failure(name + " looks like a binary file, so it cannot be read as text");
        }
        boolean truncated = size > n;
        String text = new String(buf, 0, n, StandardCharsets.UTF_8);
        // A cut can split a UTF-8 sequence; drop the replacement chars it leaves.
        if (truncated) while (text.endsWith("�")) text = text.substring(0, text.length() - 1);
        JSONObject o = new JSONObject();
        o.put("name", name);
        o.put("bytes", n);
        o.put("size", size);
        o.put("content", text);
        if (truncated) {
            o.put("truncated", true);
            o.put("note", "showing the first " + (n / 1024) + " KB of " + (size / 1024) + " KB");
        }
        return o;
    }

    private JSONObject open(JSONObject a) throws Todos.Failure, JSONException {
        File f = existing(a.optString("name", ""));
        final String name = f.getName();
        String type = viewType(name);
        String as;
        switch (kindOf(name)) {
            case "html":
                as = "a web page";
                break;
            case "text":
                as = "text";
                break;
            case "image":
                as = "an image";
                break;
            case "video":
                as = "a video";
                break;
            case "audio":
                as = "audio";
                break;
            default:
                as = type;
        }
        Uri uri = uriFor(name);
        final Intent view = new Intent(Intent.ACTION_VIEW)
                .setDataAndType(uri, type)
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION | Intent.FLAG_ACTIVITY_NEW_TASK);
        if (view.resolveActivity(ctx.getPackageManager()) == null) {
            throw new Todos.Failure("no app on this phone opens " + type + "; " + name + " is in the Files app under Documents › Toroid");
        }
        // Android 10 drops activity starts from an app with nothing on screen
        // without an error, so only open while the chat is showing.
        final MainActivity act = MainActivity.onScreen();
        if (act == null) {
            throw new Todos.Failure("Toroid Chat is not on screen, so Android will not let it open " + name
                    + "; it is in the Files app under Documents › Toroid");
        }
        final String[] err = new String[1];
        final CountDownLatch done = new CountDownLatch(1);
        act.runOnUiThread(new Runnable() {
            @Override
            public void run() {
                try {
                    act.startActivity(view);
                } catch (ActivityNotFoundException e) {
                    err[0] = "no app on this phone opens " + view.getType();
                } catch (RuntimeException e) {
                    err[0] = "could not open " + name + ": " + e.getMessage();
                } finally {
                    done.countDown();
                }
            }
        });
        try {
            if (!done.await(5, TimeUnit.SECONDS)) throw new Todos.Failure("the app did not open " + name + " in time");
        } catch (InterruptedException e) {
            throw new Todos.Failure("interrupted while opening " + name);
        }
        if (err[0] != null) throw new Todos.Failure(err[0]);
        JSONObject o = new JSONObject();
        o.put("name", name);
        o.put("opened_as", as);
        o.put("type", type);
        return o;
    }

    private synchronized JSONObject delete(JSONObject a) throws Todos.Failure, JSONException {
        File f = existing(a.optString("name", ""));
        if (!f.delete()) throw new Todos.Failure("could not delete " + f.getName());
        scan(f);
        JSONObject o = new JSONObject();
        o.put("deleted", true);
        o.put("name", f.getName());
        return o;
    }

    // ---- names and paths -----------------------------------------------------

    /** Documents/Toroid on shared storage. */
    @SuppressWarnings("deprecation") // legacy storage: the app opts in with requestLegacyExternalStorage
    static File dir() {
        return new File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOCUMENTS), "Toroid");
    }

    /**
     * Maps a name to its file, refusing anything but one plain file name
     * directly inside {@link #dir()}: the canonical path (symlinks resolved)
     * must have the directory as its parent.
     */
    static File resolve(String name) throws Todos.Failure {
        String n = name == null ? "" : name.trim().replaceAll("\\s+", " ");
        if (n.isEmpty()) throw new Todos.Failure("name is empty");
        if (!NAME.matcher(n).matches() || n.contains("..")) {
            throw new Todos.Failure("bad name \"" + n + "\": give one file name with an extension, "
                    + "using letters, digits, spaces and . _ ( ) - only");
        }
        try {
            File d = dir().getCanonicalFile();
            File f = new File(d, n).getCanonicalFile();
            if (!d.equals(f.getParentFile())) throw new Todos.Failure("bad name \"" + n + "\": it must stay in " + SHOWN_AS);
            return f;
        } catch (IOException e) {
            throw new Todos.Failure("bad name \"" + n + "\": " + e.getMessage());
        }
    }

    private static File existing(String name) throws Todos.Failure {
        File f = resolve(name);
        if (!f.isFile()) {
            throw new Todos.Failure("no file named \"" + f.getName() + "\" in " + SHOWN_AS + "; call files_list for the names");
        }
        return f;
    }

    static Uri uriFor(String name) {
        return new Uri.Builder().scheme("content").authority(AUTHORITY).appendPath(name).build();
    }

    static String ext(String name) {
        int i = name.lastIndexOf('.');
        return i <= 0 ? "" : name.substring(i + 1).toLowerCase(Locale.ROOT);
    }

    /** The file's own MIME type, from its extension. */
    static String mimeOf(String name) {
        String ext = ext(name);
        switch (ext) {
            case "html":
            case "htm":
                return "text/html";
            case "md":
            case "markdown":
                return "text/markdown";
            case "json":
                return "application/json";
            case "csv":
                return "text/csv";
        }
        String m = MimeTypeMap.getSingleton().getMimeTypeFromExtension(ext);
        if (m != null) return m;
        return TEXT.contains(ext) ? "text/plain" : "application/octet-stream";
    }

    /** html, text, image, video, audio or other. */
    static String kindOf(String name) {
        String ext = ext(name);
        if (ext.equals("html") || ext.equals("htm")) return "html";
        if (TEXT.contains(ext)) return "text";
        String m = mimeOf(name);
        if (m.startsWith("image/")) return "image";
        if (m.startsWith("video/")) return "video";
        if (m.startsWith("audio/")) return "audio";
        if (m.startsWith("text/")) return "text";
        return "other";
    }

    /** The MIME type a viewer is asked to show the file as. */
    static String viewType(String name) {
        switch (kindOf(name)) {
            case "html":
                return "text/html";
            case "text":
                return "text/plain"; // "open as a text file"
            default:
                return mimeOf(name);
        }
    }

    private static String describe(String name, long size) {
        String what;
        switch (kindOf(name)) {
            case "image":
                what = "an image";
                break;
            case "video":
                what = "a video";
                break;
            case "audio":
                what = "an audio file";
                break;
            default:
                what = "a " + mimeOf(name) + " file";
        }
        return what + " (" + Math.max(1, (size + 512) / 1024) + " KB)";
    }

    private void scan(File f) {
        MediaScannerConnection.scanFile(ctx, new String[]{f.getAbsolutePath()}, null, null);
    }
}
