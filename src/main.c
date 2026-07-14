#include "pd_api.h"
#include <stdlib.h>
#include <string.h>
#include "config.h"
// FIXME: extract shared helpers to ../playdate-shared-utils/

static int update(void* userdata);
static PlaydateAPI* gpd = NULL;
static const char* fontpath = "Tiny5";
static LCDFont* font = NULL;

static float refreshInterval = 300.0f;
static float timerElapsed = 0; // paused (stops accruing) while a fetch is in flight
#define BAR_COUNT 3
#define BAR_X 24
#define BAR_Y0 88
#define BAR_W 352
#define BAR_H 16
#define BAR_GAP 42
#define ANIM_SPEED 0.02f
#define COUNTDOWN_H 3
#define BODY_MAX 1024

typedef struct {
    char label[40];
    char resets[16];
    float target;
    float current;
    int used;
} Bar;

static Bar bars[BAR_COUNT];
static char statusMsg[64] = "loading...";
static int stale = 0;
static int haveData = 0;
static int netAllowed = 0;
static int refreshFlash = 0;
static int limitHit = 0;         // session bar at 100%
static float resetWait = 0;      // seconds from last fetch until the session resets
static char limitMsg[64] = "";
static int autoLockDirty = 0;    // set from the network callback, applied in update()
static PDMenuItem* timerMenuItem = NULL;
static PDMenuItem* noSleepMenuItem = NULL;
static PDMenuItem* sleepAtFullMenuItem = NULL;
static PDSynth* dingSynth = NULL;
static int dingPending = 0;
static HTTPConnection* conn = NULL;
static float retryIn = 0; // when >0, retry a failed fetch after this many seconds

#define FORCE_HOLD_MS 1500
static unsigned int aHeldSince = 0; // ms timestamp when A went down, 0 = not held

static void startFetch(void);

static void drawBar(PlaydateAPI* pd, int x, int y, int w, int h, float pct)
{
    pd->graphics->drawRect(x - 1, y - 1, w + 2, h + 2, kColorBlack);

    int fillW = (int)(w * pct);
    if (fillW >= 1)
        pd->graphics->fillRect(x, y, fillW, h, kColorBlack);
}

// parseResets converts the server's pretty duration ("3d 18h", "1h 57m",
// "57m") to seconds; returns 0 when nothing parses.
static float parseResets(const char* s)
{
    float secs = 0;
    while (*s)
    {
        if (*s >= '0' && *s <= '9')
        {
            long n = strtol(s, (char**)&s, 10);
            if (*s == 'd') secs += n * 86400;
            else if (*s == 'h') secs += n * 3600;
            else if (*s == 'm') secs += n * 60;
        }
        else
            s++;
    }
    return secs;
}

// While the session bar sits at 100% the normal cadence stops and the next
// fetch waits for the reset itself (plus a buffer, since the server's reset
// string has minute resolution).
#define RESET_BUFFER 60.0f
static float fetchInterval(void)
{
    if (limitHit && resetWait > 0)
        return resetWait < 60.0f ? 60.0f : resetWait;
    return refreshInterval;
}

// parseBody consumes the server's plain format (see backend writePlain):
//   ok            (or "stale")
//   session|63|1h 57m
//   weekly|59|3d 18h
//   Fable|6|3d 18h
static void parseBody(char* body)
{
    stale = 0;
    int bi = 0;
    char* line = strtok(body, "\n");
    if (line != NULL && strcmp(line, "stale") == 0)
        stale = 1;

    while ((line = strtok(NULL, "\n")) != NULL && bi < BAR_COUNT)
    {
        char* p1 = strchr(line, '|');
        if (p1 == NULL) continue;
        char* p2 = strchr(p1 + 1, '|');
        if (p2 == NULL) continue;
        *p1 = '\0';
        *p2 = '\0';

        const char* name = line;
        int isSession = strcmp(name, "session") == 0;
        if (isSession) name = "Current session";
        else if (strcmp(name, "weekly") == 0) name = "All models";

        Bar* b = &bars[bi];
        snprintf(b->label, sizeof(b->label), "%s", name);
        snprintf(b->resets, sizeof(b->resets), "%s", p2 + 1);
        b->target = atoi(p1 + 1) / 100.0f;
        if (b->current > b->target)
            b->current = 0; // re-animate when the bar shrinks (new session)
        b->used = 1;
        bi++;

        if (isSession)
        {
            limitHit = b->target >= 1.0f;
            resetWait = 0;
            limitMsg[0] = '\0';
            if (limitHit)
            {
                resetWait = parseResets(b->resets);
                if (resetWait > 0)
                    resetWait += RESET_BUFFER;
                snprintf(limitMsg, sizeof(limitMsg),
                         "limit hit - next refresh at reset (%s)", b->resets);
            }
            autoLockDirty = 1;
        }
    }
    statusMsg[0] = '\0';
    refreshFlash = 25;
    haveData = 1;
    // played from update(); this runs on the network callback
    // 2 = limit hit: two-note minor fall instead of the usual ding
    dingPending = limitHit ? 2 : 1;
}

static void requestComplete(HTTPConnection* c)
{
    PlaydateAPI* pd = gpd;
    int status = pd->network->http->getResponseStatus(c);
    if (status == 200)
    {
        static char body[BODY_MAX];
        int total = 0;
        while (total < BODY_MAX - 1)
        {
            int n = pd->network->http->read(c, body + total, BODY_MAX - 1 - total);
            if (n <= 0) break;
            total += n;
        }
        body[total] = '\0';
        parseBody(body);
    }
    else
    {
        PDNetErr err = pd->network->http->getError(c);
        snprintf(statusMsg, sizeof(statusMsg), "HTTP %d (net %d)", status, err);
        retryIn = 15.0f;
    }
    pd->network->http->close(c);
    pd->network->http->release(c);
    if (c == conn)
        conn = NULL;
}

static void startFetch(void)
{
    PlaydateAPI* pd = gpd;
    if (conn != NULL || !netAllowed)
        return;
    timerElapsed = 0;
    retryIn = 0;

    conn = pd->network->http->newConnection(SERVER_HOST, 443, true);
    if (conn == NULL)
    {
        snprintf(statusMsg, sizeof(statusMsg), "no connection");
        return;
    }
    pd->network->http->setConnectTimeout(conn, 10000);
    pd->network->http->setReadTimeout(conn, 10000);
    pd->network->http->setRequestCompleteCallback(conn, requestComplete);

    static const char headers[] = "Authorization: Bearer " AUTH_TOKEN "\r\n";
    PDNetErr err = pd->network->http->get(conn, "/api/usage?format=plain", headers, strlen(headers));
    if (err != NET_OK)
    {
        if (err == NET_NO_DEVICE)
            snprintf(statusMsg, sizeof(statusMsg), "no network (simulator: enable it in Settings)");
        else
            snprintf(statusMsg, sizeof(statusMsg), "net err %d", err);
        pd->network->http->release(conn);
        conn = NULL;
        retryIn = 5.0f; // transient (wifi warming up, sim toggle) — retry soon
    }
}

// Firmware (3.0.6) calls this without a NULL check, so passing NULL to
// setEnabled crashes the device with "Error accessing buffer at 0x0".
static void netEnabledCallback(PDNetErr err)
{
    if (err != NET_OK && statusMsg[0] == '\0')
        snprintf(statusMsg, sizeof(statusMsg), "wifi err %d", err);
}

static void accessCallback(bool allowed, void* userdata)
{
    (void)userdata;
    netAllowed = allowed;
    if (allowed)
        startFetch();
    else
        snprintf(statusMsg, sizeof(statusMsg), "network access denied");
}

static void timerOptionCallback(void* userdata)
{
    (void)userdata;
    int val = gpd->system->getMenuItemValue(timerMenuItem);
    refreshInterval = (val == 0) ? 60.0f : 300.0f;
    timerElapsed = 0;
}

// "No Sleep" keeps the screen on, except when "Sleep at 100%" is checked and
// the session limit is hit — then auto-lock comes back so the device can
// sleep in low power until the reset.
static void applyAutoLock(void)
{
    int noSleep = gpd->system->getMenuItemValue(noSleepMenuItem);
#if TARGET_SIMULATOR
    // The simulator's auto-lock just blanks the window ("shows nothing"),
    // so sleeping at the limit is device-only.
    gpd->system->setAutoLockDisabled(noSleep);
#else
    int sleepAtFull = gpd->system->getMenuItemValue(sleepAtFullMenuItem);
    gpd->system->setAutoLockDisabled(noSleep && !(limitHit && sleepAtFull));
#endif
}

static void noSleepCallback(void* userdata)
{
    (void)userdata;
    applyAutoLock();
}

static void sleepAtFullCallback(void* userdata)
{
    (void)userdata;
    applyAutoLock();
}

int eventHandler(PlaydateAPI* pd, PDSystemEvent event, uint32_t arg)
{
    (void)arg;

    if (event == kEventInit)
    {
        gpd = pd;
        const char* err;
        font = pd->graphics->loadFont(fontpath, &err);
        if (font == NULL)
            pd->system->error("Couldn't load font: %s", err);

        dingSynth = pd->sound->synth->newSynth();
        pd->sound->synth->setWaveform(dingSynth, kWaveformSine);
        pd->sound->synth->setAttackTime(dingSynth, 0.001f);
        pd->sound->synth->setDecayTime(dingSynth, 0.15f);
        pd->sound->synth->setSustainLevel(dingSynth, 0.0f);
        pd->sound->synth->setReleaseTime(dingSynth, 0.05f);
        pd->sound->channel->addSource(pd->sound->getDefaultChannel(), (SoundSource*)dingSynth);

        const char* opts[] = {"1 min", "5 min"};
        timerMenuItem = pd->system->addOptionsMenuItem("Interval", opts, 2, timerOptionCallback, NULL);
        pd->system->setMenuItemValue(timerMenuItem, 1); // default 5 min

        noSleepMenuItem = pd->system->addCheckmarkMenuItem("No Sleep", 1, noSleepCallback, NULL);
        // "%" gets eaten by the system menu renderer, so no "100%" in the title
        sleepAtFullMenuItem = pd->system->addCheckmarkMenuItem("Sleep on limit", 1, sleepAtFullCallback, NULL);
        pd->system->setAutoLockDisabled(1);

        pd->system->resetElapsedTime();
        pd->system->setUpdateCallback(update, pd);
    }
    return 0;
}

static int update(void* userdata)
{
    PlaydateAPI* pd = userdata;
    float dt = pd->system->getElapsedTime();
    pd->system->resetElapsedTime();
    if (conn == NULL)
        timerElapsed += dt;

    // The permission dialog can't be shown at load time (the system pauses
    // the game and deadlocks), so network setup waits for the first frame.
    static int firstFrame = 1;
    if (firstFrame)
    {
        firstFrame = 0;
        // Start the AP connection now (takes up to ~10s on hardware) instead
        // of lazily on the first request.
        pd->network->setEnabled(true, netEnabledCallback);
        enum accessReply reply = pd->network->http->requestAccess(
            SERVER_HOST, 443, true, "Show Claude plan usage limits",
            accessCallback, NULL);
        if (reply == kAccessAllow)
        {
            netAllowed = 1;
            startFetch();
        }
        else if (reply == kAccessDeny)
            snprintf(statusMsg, sizeof(statusMsg), "network access denied");
        // kAccessAsk: accessCallback fires after the user answers the dialog
    }

    if (timerElapsed >= fetchInterval() || (retryIn > 0 && timerElapsed >= retryIn))
        startFetch();

    if (autoLockDirty)
    {
        autoLockDirty = 0;
        applyAutoLock();
    }

    if (dingPending)
    {
        if (dingPending == 2)
        {
            // limit hit: descending minor third (F#5 -> D#5), "no more" cue
            uint32_t at = pd->sound->getCurrentTime();
            pd->sound->synth->playNote(dingSynth, 739.99f, 0.6f, 0.2f, at);
            pd->sound->synth->playNote(dingSynth, 622.25f, 0.6f, 0.35f, at + 11025);
        }
        else
            pd->sound->synth->playNote(dingSynth, 880.0f, 0.6f, 0.2f, 0);
        dingPending = 0;
    }

    // Hold A for 1.5 seconds to force a refresh.
    float holdPct = 0;
    PDButtons held;
    pd->system->getButtonState(&held, NULL, NULL);
    if ((held & kButtonA) && conn == NULL)
    {
        unsigned int now = pd->system->getCurrentTimeMilliseconds();
        if (aHeldSince == 0)
            aHeldSince = now;
        else if (now - aHeldSince >= FORCE_HOLD_MS)
        {
            aHeldSince = 0;
            startFetch();
        }
        if (aHeldSince != 0)
            holdPct = (float)(now - aHeldSince) / FORCE_HOLD_MS;
    }
    else
        aHeldSince = 0;

    pd->graphics->clear(kColorWhite);
    pd->graphics->setFont(font);

    pd->graphics->fillRect(0, 0, LCD_COLUMNS, 26, kColorBlack);
    pd->graphics->setDrawMode(kDrawModeFillWhite);
    pd->graphics->drawText("Claude Session", strlen("Claude Session"), kASCIIEncoding, 6, 3);
    if (stale)
    {
        const char* s = "stale";
        int sw = pd->graphics->getTextWidth(font, s, strlen(s), kASCIIEncoding, 0);
        pd->graphics->drawText(s, strlen(s), kASCIIEncoding, LCD_COLUMNS - sw - 6, 3);
    }
    if (holdPct > 0)
    {
        // Hold-to-refresh progress, right side of the header bar.
        const int pw = 80, ph = 8;
        int px = LCD_COLUMNS - pw - 6, py = (26 - ph) / 2;
        pd->graphics->drawRect(px, py, pw, ph, kColorWhite);
        pd->graphics->fillRect(px, py, (int)(pw * holdPct), ph, kColorWhite);
    }
    pd->graphics->setDrawMode(kDrawModeCopy);

    int textH = pd->graphics->getFontHeight(font);
    for (int i = 0; i < BAR_COUNT; i++)
    {
        Bar* b = &bars[i];
        if (!b->used) continue;
        int by = BAR_Y0 + i * BAR_GAP;

        if (b->current < b->target)
        {
            b->current += ANIM_SPEED;
            if (b->current > b->target)
                b->current = b->target;
        }

        char left[80];
        if (b->resets[0] != '\0')
            snprintf(left, sizeof(left), "%s  (resets %s)", b->label, b->resets);
        else
            snprintf(left, sizeof(left), "%s", b->label);

        char pct[8];
        snprintf(pct, sizeof(pct), "%d%%", (int)(b->current * 100));
        int pctW = pd->graphics->getTextWidth(font, pct, strlen(pct), kASCIIEncoding, 0);

        pd->graphics->drawText(left, strlen(left), kASCIIEncoding, BAR_X, by - textH - 2);
        pd->graphics->drawText(pct, strlen(pct), kASCIIEncoding, BAR_X + BAR_W - pctW, by - textH - 2);
        drawBar(pd, BAR_X, by, BAR_W, BAR_H + 2, b->current);
    }

    const char* info = statusMsg[0] != '\0' ? statusMsg
                     : (limitHit && limitMsg[0] != '\0') ? limitMsg
                     : NULL;
    if (info != NULL)
    {
        int mw = pd->graphics->getTextWidth(font, info, strlen(info), kASCIIEncoding, 0);
        pd->graphics->drawText(info, strlen(info), kASCIIEncoding, (LCD_COLUMNS - mw) / 2, 60);
    }
    else if (conn != NULL || refreshFlash > 0)
    {
        const char* msg = conn != NULL ? "refreshing..." : "updated";
        int mw = pd->graphics->getTextWidth(font, msg, strlen(msg), kASCIIEncoding, 0);
        pd->graphics->drawText(msg, strlen(msg), kASCIIEncoding, 400 - mw - 4, 28);
        if (conn == NULL)
            refreshFlash--;
    }

    if (haveData)
    {
        float interval = fetchInterval();
        float remain = interval - timerElapsed;
        if (remain < 0) remain = 0;
        int cw = (int)(LCD_COLUMNS * (remain / interval));
        if (cw > 0)
            pd->graphics->fillRect(0, LCD_ROWS - COUNTDOWN_H, cw, COUNTDOWN_H, kColorBlack);
    }

    return 1;
}
