package com.toroid.chat;

import android.Manifest;
import android.content.ContentResolver;
import android.content.ContentUris;
import android.content.ContentValues;
import android.content.Context;
import android.content.pm.PackageManager;
import android.database.Cursor;
import android.net.Uri;
import android.provider.CalendarContract;
import android.provider.CalendarContract.Calendars;
import android.provider.CalendarContract.Events;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.time.Instant;
import java.time.LocalDate;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.time.format.DateTimeParseException;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.List;
import java.util.TimeZone;

/**
 * The to-do list, kept in the phone's calendar so it shows in the Calendar app.
 *
 * <p>To-dos live in a local calendar named "Toroid", created on first use. Each
 * to-do is an all-day event on its due date. The event's title is the to-do (a
 * done one gets a leading "✓ "), and its description holds the status on the
 * first line, exactly "status: open" or "status: done", then the notes.
 *
 * <p>Ops only ever touch events in the Toroid calendar: an id from any other
 * calendar is answered as "no to-do with id N".
 */
final class Todos {
    static final String ACCOUNT = "Toroid";
    private static final int COLOR = 0xFF7CC4A4; // the app's accent
    private static final String DONE_MARK = "\u2713 "; // ✓
    private static final String[] PROJECTION = {
            Events._ID, Events.TITLE, Events.DTSTART, Events.ALL_DAY, Events.DESCRIPTION,
    };

    /** A refusal whose message is meant for the model, and through it the user. */
    static final class Failure extends Exception {
        Failure(String msg) {
            super(msg);
        }
    }

    private final Context ctx;
    private final ContentResolver cr;

    Todos(Context ctx) {
        this.ctx = ctx.getApplicationContext();
        this.cr = this.ctx.getContentResolver();
    }

    /** Runs one op; the result is a JSONObject or JSONArray. */
    Object run(String op, JSONObject a) throws Failure, JSONException {
        if (ctx.checkSelfPermission(Manifest.permission.READ_CALENDAR) != PackageManager.PERMISSION_GRANTED
                || ctx.checkSelfPermission(Manifest.permission.WRITE_CALENDAR) != PackageManager.PERMISSION_GRANTED) {
            throw new Failure("Toroid Chat has no calendar permission, so it cannot reach the to-do list. "
                    + "Open the app and allow Calendar access, or turn it on in Settings > Apps > Toroid Chat > Permissions.");
        }
        switch (op) {
            case "tasks_list":
                return list(a);
            case "task_add":
                return add(a);
            case "task_update":
                return update(a);
            case "task_done":
                return done(a);
            case "task_delete":
                return delete(a);
            default:
                throw new Failure("unknown op: " + op);
        }
    }

    // ---- ops ----------------------------------------------------------------

    private JSONArray list(JSONObject a) throws Failure, JSONException {
        boolean includeDone = a.optBoolean("include_done", false);
        LocalDate from = a.has("from") ? parseDate("from", a.getString("from")) : null;
        LocalDate to = a.has("to") ? parseDate("to", a.getString("to")) : null;
        long cal = calendarId();
        List<Todo> all = new ArrayList<>();
        Cursor c = cr.query(Events.CONTENT_URI, PROJECTION,
                Events.CALENDAR_ID + "=? AND " + Events.DELETED + "=0",
                new String[]{String.valueOf(cal)}, null);
        if (c != null) {
            try {
                while (c.moveToNext()) {
                    Todo t = Todo.from(c);
                    if (!includeDone && t.done) continue;
                    if (from != null && t.due.isBefore(from)) continue;
                    if (to != null && t.due.isAfter(to)) continue;
                    all.add(t);
                }
            } finally {
                c.close();
            }
        }
        Collections.sort(all, new Comparator<Todo>() {
            @Override
            public int compare(Todo x, Todo y) {
                int d = x.due.compareTo(y.due);
                return d != 0 ? d : Long.compare(x.id, y.id);
            }
        });
        JSONArray out = new JSONArray();
        for (Todo t : all) out.put(t.json());
        return out;
    }

    private JSONObject add(JSONObject a) throws Failure, JSONException {
        String title = cleanTitle(a.optString("title", ""));
        LocalDate due = a.has("due") ? parseDate("due", a.getString("due")) : LocalDate.now();
        String notes = a.optString("notes", "").trim();
        ContentValues v = new ContentValues();
        v.put(Events.CALENDAR_ID, calendarId());
        v.put(Events.TITLE, title);
        v.put(Events.DESCRIPTION, description(false, notes));
        putDue(v, due);
        Uri row = cr.insert(Events.CONTENT_URI, v);
        if (row == null) throw new Failure("the calendar did not save the to-do");
        return get(ContentUris.parseId(row)).json();
    }

    private JSONObject update(JSONObject a) throws Failure, JSONException {
        Todo t = get(a.getLong("id"));
        ContentValues v = new ContentValues();
        if (a.has("title")) {
            t.title = cleanTitle(a.getString("title"));
            v.put(Events.TITLE, (t.done ? DONE_MARK : "") + t.title);
        }
        if (a.has("notes")) {
            t.notes = a.getString("notes").trim();
            v.put(Events.DESCRIPTION, description(t.done, t.notes));
        }
        if (a.has("due")) putDue(v, parseDate("due", a.getString("due")));
        if (v.size() == 0) throw new Failure("nothing to change: give a title, due or notes");
        write(t.id, v);
        return get(t.id).json();
    }

    private JSONObject done(JSONObject a) throws Failure, JSONException {
        Todo t = get(a.getLong("id"));
        ContentValues v = new ContentValues();
        v.put(Events.TITLE, DONE_MARK + t.title);
        v.put(Events.DESCRIPTION, description(true, t.notes));
        write(t.id, v);
        return get(t.id).json();
    }

    private JSONObject delete(JSONObject a) throws Failure, JSONException {
        Todo t = get(a.getLong("id"));
        // An event never synced (no _sync_id, as in a local calendar) is
        // removed outright rather than flagged for a sync adapter.
        if (cr.delete(ContentUris.withAppendedId(Events.CONTENT_URI, t.id), null, null) == 0) {
            throw new Failure("the calendar did not delete to-do " + t.id);
        }
        JSONObject out = new JSONObject();
        out.put("deleted", true);
        out.put("task", t.json());
        return out;
    }

    // ---- storage --------------------------------------------------------------

    /** Returns the Toroid calendar's id, creating the calendar on first use. */
    private synchronized long calendarId() throws Failure {
        Cursor c = cr.query(Calendars.CONTENT_URI, new String[]{Calendars._ID},
                Calendars.ACCOUNT_NAME + "=? AND " + Calendars.ACCOUNT_TYPE + "=?",
                new String[]{ACCOUNT, CalendarContract.ACCOUNT_TYPE_LOCAL}, Calendars._ID);
        if (c != null) {
            try {
                if (c.moveToFirst()) return c.getLong(0);
            } finally {
                c.close();
            }
        }
        // Only a sync adapter may create a calendar; a local account needs no
        // real adapter behind it.
        Uri uri = Calendars.CONTENT_URI.buildUpon()
                .appendQueryParameter(CalendarContract.CALLER_IS_SYNCADAPTER, "true")
                .appendQueryParameter(Calendars.ACCOUNT_NAME, ACCOUNT)
                .appendQueryParameter(Calendars.ACCOUNT_TYPE, CalendarContract.ACCOUNT_TYPE_LOCAL)
                .build();
        ContentValues v = new ContentValues();
        v.put(Calendars.ACCOUNT_NAME, ACCOUNT);
        v.put(Calendars.ACCOUNT_TYPE, CalendarContract.ACCOUNT_TYPE_LOCAL);
        v.put(Calendars.NAME, ACCOUNT);
        v.put(Calendars.CALENDAR_DISPLAY_NAME, ACCOUNT);
        v.put(Calendars.CALENDAR_COLOR, COLOR);
        v.put(Calendars.CALENDAR_ACCESS_LEVEL, Calendars.CAL_ACCESS_OWNER);
        v.put(Calendars.OWNER_ACCOUNT, ACCOUNT);
        v.put(Calendars.VISIBLE, 1);
        v.put(Calendars.SYNC_EVENTS, 1);
        v.put(Calendars.CALENDAR_TIME_ZONE, TimeZone.getDefault().getID());
        Uri row = cr.insert(uri, v);
        if (row == null) throw new Failure("could not create the Toroid calendar");
        return ContentUris.parseId(row);
    }

    /** Loads one to-do of the Toroid calendar. */
    private Todo get(long id) throws Failure {
        Cursor c = cr.query(Events.CONTENT_URI, PROJECTION,
                Events._ID + "=? AND " + Events.CALENDAR_ID + "=? AND " + Events.DELETED + "=0",
                new String[]{String.valueOf(id), String.valueOf(calendarId())}, null);
        if (c != null) {
            try {
                if (c.moveToFirst()) return Todo.from(c);
            } finally {
                c.close();
            }
        }
        throw new Failure("no to-do with id " + id + "; call tasks_list for current ids");
    }

    private void write(long id, ContentValues v) throws Failure {
        if (cr.update(ContentUris.withAppendedId(Events.CONTENT_URI, id), v, null, null) == 0) {
            throw new Failure("the calendar did not update to-do " + id);
        }
    }

    /**
     * Makes the event all-day on due. CalendarContract wants all-day events at
     * UTC midnight, ending at the next UTC midnight, in the UTC time zone.
     */
    private static void putDue(ContentValues v, LocalDate due) {
        long start = due.atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli();
        long end = due.plusDays(1).atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli();
        v.put(Events.DTSTART, start);
        v.put(Events.DTEND, end);
        v.put(Events.ALL_DAY, 1);
        v.put(Events.EVENT_TIMEZONE, "UTC");
    }

    private static String description(boolean done, String notes) {
        String status = done ? "status: done" : "status: open";
        return notes.isEmpty() ? status : status + "\n" + notes;
    }

    private static String cleanTitle(String t) throws Failure {
        t = t.trim().replaceAll("\\s+", " ");
        if (t.startsWith(DONE_MARK)) t = t.substring(DONE_MARK.length()).trim();
        if (t.isEmpty()) throw new Failure("title is empty");
        if (t.codePointCount(0, t.length()) > 200) throw new Failure("title is longer than 200 characters");
        return t;
    }

    private static LocalDate parseDate(String field, String s) throws Failure {
        try {
            return LocalDate.parse(s); // ISO: YYYY-MM-DD only
        } catch (DateTimeParseException e) {
            throw new Failure("bad " + field + " date \"" + s + "\": use YYYY-MM-DD");
        }
    }

    /** One to-do as read from an event row. */
    private static final class Todo {
        long id;
        String title;
        LocalDate due;
        boolean done;
        String notes;

        static Todo from(Cursor c) {
            Todo t = new Todo();
            t.id = c.getLong(0);
            String title = c.isNull(1) ? "" : c.getString(1);
            long start = c.getLong(2);
            boolean allDay = c.getInt(3) != 0;
            String desc = c.isNull(4) ? "" : c.getString(4);
            // All-day dates are UTC; an event added by hand with a time falls
            // on its local date.
            t.due = Instant.ofEpochMilli(start).atZone(allDay ? ZoneOffset.UTC : ZoneId.systemDefault()).toLocalDate();
            boolean marked = title.startsWith(DONE_MARK);
            t.title = marked ? title.substring(DONE_MARK.length()) : title;
            if (desc.startsWith("status:")) {
                int nl = desc.indexOf('\n');
                String status = (nl < 0 ? desc : desc.substring(0, nl)).substring("status:".length()).trim();
                t.done = status.equals("done");
                t.notes = nl < 0 ? "" : desc.substring(nl + 1).trim();
            } else {
                // Made in the Calendar app: no status line yet.
                t.done = marked;
                t.notes = desc.trim();
            }
            return t;
        }

        JSONObject json() throws JSONException {
            JSONObject o = new JSONObject();
            o.put("id", id);
            o.put("title", title);
            o.put("due", due.toString());
            o.put("status", done ? "done" : "open");
            if (!notes.isEmpty()) o.put("notes", notes);
            return o;
        }
    }
}
