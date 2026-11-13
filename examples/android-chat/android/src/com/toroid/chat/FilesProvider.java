package com.toroid.chat;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.ParcelFileDescriptor;
import android.provider.OpenableColumns;

import java.io.File;
import java.io.FileNotFoundException;
import java.util.List;

/**
 * Read-only access to the files in Documents/Toroid for the viewer that
 * file_open starts, as content://com.toroid.chat.files/&lt;name&gt;. Not
 * exported: a viewer gets one file through the intent's read grant, and
 * nothing else. androidx's FileProvider does the same job, but this build has
 * no androidx.
 */
public final class FilesProvider extends ContentProvider {
    private static final String[] COLUMNS = {OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE};

    @Override
    public boolean onCreate() {
        return true;
    }

    /** The one file uri names, or null. */
    private static File fileOf(Uri uri) {
        List<String> segs = uri.getPathSegments();
        if (segs.size() != 1) return null;
        try {
            File f = Files.resolve(segs.get(0));
            return f.isFile() ? f : null;
        } catch (Todos.Failure e) {
            return null;
        }
    }

    @Override
    public String getType(Uri uri) {
        File f = fileOf(uri);
        return f == null ? null : Files.viewType(f.getName());
    }

    @Override
    public Cursor query(Uri uri, String[] projection, String selection, String[] selectionArgs, String sortOrder) {
        File f = fileOf(uri);
        String[] cols = projection != null ? projection : COLUMNS;
        MatrixCursor c = new MatrixCursor(cols, 1);
        if (f == null) return c;
        Object[] row = new Object[cols.length];
        for (int i = 0; i < cols.length; i++) {
            if (OpenableColumns.DISPLAY_NAME.equals(cols[i])) row[i] = f.getName();
            else if (OpenableColumns.SIZE.equals(cols[i])) row[i] = f.length();
        }
        c.addRow(row);
        return c;
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws FileNotFoundException {
        if (!"r".equals(mode)) throw new SecurityException("read-only");
        File f = fileOf(uri);
        if (f == null) throw new FileNotFoundException(uri.toString());
        return ParcelFileDescriptor.open(f, ParcelFileDescriptor.MODE_READ_ONLY);
    }

    @Override
    public Uri insert(Uri uri, ContentValues values) {
        throw new UnsupportedOperationException("read-only");
    }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) {
        throw new UnsupportedOperationException("read-only");
    }

    @Override
    public int delete(Uri uri, String selection, String[] selectionArgs) {
        throw new UnsupportedOperationException("read-only");
    }
}
