package com.drseussfreak.backtrackarr;

import android.app.Activity;
import android.app.AlertDialog;
import android.app.DownloadManager;
import android.content.ActivityNotFoundException;
import android.content.ClipData;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.graphics.Color;
import android.net.Uri;
import android.os.Bundle;
import android.os.Environment;
import android.text.InputType;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowInsets;
import android.webkit.CookieManager;
import android.webkit.URLUtil;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceResponse;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

/** A thin wrapper around a self-hosted BackTrackarr server. */
public class MainActivity extends Activity {
    private static final String CHANGE_SERVER = "com.drseussfreak.backtrackarr.CHANGE_SERVER";
    private static final int PICK_FILES = 1;

    private SharedPreferences prefs;
    private WebView web;
    private LinearLayout errorView;
    private TextView errorText;
    private ValueCallback<Uri[]> filePicked;
    private boolean mainFrameFailed;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        prefs = getSharedPreferences("backtrackarr", Context.MODE_PRIVATE);

        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(Color.parseColor("#141413"));
        if (android.os.Build.VERSION.SDK_INT >= 30) {
            root.setOnApplyWindowInsetsListener(new View.OnApplyWindowInsetsListener() {
                @Override
                public WindowInsets onApplyWindowInsets(View v, WindowInsets insets) {
                    android.graphics.Insets i = insets.getInsets(WindowInsets.Type.systemBars()
                            | WindowInsets.Type.displayCutout() | WindowInsets.Type.ime());
                    v.setPadding(i.left, i.top, i.right, i.bottom);
                    return WindowInsets.CONSUMED;
                }
            });
        } else {
            root.setFitsSystemWindows(true);
        }

        web = new WebView(this);
        web.setBackgroundColor(Color.parseColor("#141413"));
        root.addView(web, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        errorView = buildErrorView();
        errorView.setVisibility(View.GONE);
        root.addView(errorView, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        setContentView(root);

        configureWeb();
        boolean change = CHANGE_SERVER.equals(getIntent().getAction());
        if (state != null) {
            web.restoreState(state);
        }
        if (server().isEmpty() || change) {
            askForServer();
        } else if (state == null) {
            web.loadUrl(server());
        }
    }

    private String server() {
        return prefs.getString("server", Defaults.SERVER);
    }

    private void configureWeb() {
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setAllowFileAccess(false);
        s.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        s.setSupportMultipleWindows(false);
        s.setUserAgentString(s.getUserAgentString() + " BackTrackarrApp/1.0");

        CookieManager cookies = CookieManager.getInstance();
        cookies.setAcceptCookie(true);
        cookies.setAcceptThirdPartyCookies(web, false);

        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                Uri target = request.getUrl();
                if (sameServer(target)) {
                    return false;
                }
                openOutside(target);
                return true;
            }

            @Override
            public void onPageStarted(WebView view, String url, android.graphics.Bitmap icon) {
                mainFrameFailed = false;
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                if (!mainFrameFailed) {
                    errorView.setVisibility(View.GONE);
                    CookieManager.getInstance().flush();
                }
            }

            @Override
            public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
                if (request.isForMainFrame()) {
                    showError("Can't reach your BackTrackarr server. Check your connection and the server address.");
                }
            }

            @Override
            public void onReceivedHttpError(WebView view, WebResourceRequest request, WebResourceResponse response) {
                int code = response.getStatusCode();
                if (request.isForMainFrame() && code >= 502 && code <= 504) {
                    showError("Your BackTrackarr server is restarting or offline. Try again in a moment.");
                }
            }
        });

        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback, FileChooserParams params) {
                if (filePicked != null) {
                    filePicked.onReceiveValue(null);
                }
                filePicked = callback;
                Intent pick = new Intent(Intent.ACTION_OPEN_DOCUMENT);
                pick.addCategory(Intent.CATEGORY_OPENABLE);
                pick.setType("*/*"); // Android has no type for many export formats
                pick.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, params.getMode() != FileChooserParams.MODE_OPEN);
                try {
                    startActivityForResult(pick, PICK_FILES);
                } catch (ActivityNotFoundException e) {
                    filePicked = null;
                    callback.onReceiveValue(null);
                    Toast.makeText(MainActivity.this, "No file picker is available", Toast.LENGTH_LONG).show();
                }
                return true;
            }
        });

        web.setDownloadListener(new android.webkit.DownloadListener() {
            @Override
            public void onDownloadStart(String url, String agent, String disposition, String mime, long length) {
                Uri target = Uri.parse(url);
                if (!sameServer(target)) {
                    openOutside(target);
                    return;
                }
                String name = URLUtil.guessFileName(url, disposition, mime);
                DownloadManager.Request req = new DownloadManager.Request(target);
                String cookie = CookieManager.getInstance().getCookie(url);
                if (cookie != null) {
                    req.addRequestHeader("Cookie", cookie);
                }
                req.addRequestHeader("User-Agent", agent);
                req.setTitle(name);
                req.setMimeType(mime);
                req.setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED);
                req.setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, name);
                ((DownloadManager) getSystemService(DOWNLOAD_SERVICE)).enqueue(req);
                Toast.makeText(MainActivity.this, "Downloading " + name, Toast.LENGTH_SHORT).show();
            }
        });
    }

    private boolean sameServer(Uri target) {
        Uri home = Uri.parse(server());
        return home.getHost() != null && home.getHost().equalsIgnoreCase(target.getHost())
                && String.valueOf(home.getScheme()).equalsIgnoreCase(target.getScheme())
                && home.getPort() == target.getPort();
    }

    private void openOutside(Uri target) {
        String scheme = target.getScheme();
        if (!"http".equalsIgnoreCase(scheme) && !"https".equalsIgnoreCase(scheme)) {
            return; // pages can't launch other apps through custom links
        }
        try {
            startActivity(new Intent(Intent.ACTION_VIEW, target));
        } catch (ActivityNotFoundException e) {
            Toast.makeText(this, "No browser is available", Toast.LENGTH_LONG).show();
        }
    }

    private void showError(String message) {
        mainFrameFailed = true;
        errorText.setText(message);
        errorView.setVisibility(View.VISIBLE);
    }

    private LinearLayout buildErrorView() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setGravity(Gravity.CENTER);
        box.setBackgroundColor(Color.parseColor("#141413"));
        int pad = dp(28);
        box.setPadding(pad, pad, pad, pad);
        box.setClickable(true);

        TextView title = new TextView(this);
        title.setText("BackTrackarr");
        title.setTextSize(24);
        title.setTextColor(Color.parseColor("#ECEBE6"));
        title.setGravity(Gravity.CENTER);
        box.addView(title);

        errorText = new TextView(this);
        errorText.setTextSize(16);
        errorText.setTextColor(Color.parseColor("#9A9A93"));
        errorText.setGravity(Gravity.CENTER);
        errorText.setPadding(0, dp(12), 0, dp(20));
        box.addView(errorText);

        Button retry = new Button(this);
        retry.setText("Try again");
        retry.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                errorView.setVisibility(View.GONE);
                String current = web.getUrl();
                web.loadUrl(current != null && sameServer(Uri.parse(current)) ? current : server());
            }
        });
        box.addView(retry);

        Button change = new Button(this);
        change.setText("Change server");
        change.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                askForServer();
            }
        });
        box.addView(change);
        return box;
    }

    private int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }

    private void askForServer() {
        final EditText input = new EditText(this);
        input.setHint("https://backtrackarr.example.com");
        input.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        input.setSingleLine(true);
        input.setText(server());
        FrameLayout wrap = new FrameLayout(this);
        int pad = dp(20);
        wrap.setPadding(pad, dp(8), pad, 0);
        wrap.addView(input);

        final AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle("BackTrackarr server")
                .setMessage("Enter the address you use to open BackTrackarr.")
                .setView(wrap)
                .setCancelable(!server().isEmpty())
                .setPositiveButton("Connect", null)
                .create();
        dialog.show();
        dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                String address = normalize(input.getText().toString());
                if (address == null) {
                    input.setError("Enter a full address, such as https://example.com");
                    return;
                }
                prefs.edit().putString("server", address).apply();
                dialog.dismiss();
                errorView.setVisibility(View.GONE);
                web.loadUrl(address);
            }
        });
    }

    private static String normalize(String raw) {
        String v = raw.trim();
        while (v.endsWith("/")) {
            v = v.substring(0, v.length() - 1);
        }
        if (v.isEmpty()) {
            return null;
        }
        if (!v.contains("://")) {
            v = "https://" + v;
        }
        Uri u = Uri.parse(v);
        String scheme = u.getScheme();
        if (u.getHost() == null || !("http".equalsIgnoreCase(scheme) || "https".equalsIgnoreCase(scheme))) {
            return null;
        }
        return v;
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        if (CHANGE_SERVER.equals(intent.getAction())) {
            askForServer();
        }
    }

    @Override
    protected void onActivityResult(int request, int result, Intent data) {
        if (request != PICK_FILES || filePicked == null) {
            super.onActivityResult(request, result, data);
            return;
        }
        Uri[] picked = null;
        if (result == RESULT_OK && data != null) {
            ClipData many = data.getClipData();
            if (many != null) {
                picked = new Uri[many.getItemCount()];
                for (int i = 0; i < picked.length; i++) {
                    picked[i] = many.getItemAt(i).getUri();
                }
            } else if (data.getData() != null) {
                picked = new Uri[] {data.getData()};
            }
        }
        filePicked.onReceiveValue(picked);
        filePicked = null;
    }

    @Override
    public void onBackPressed() {
        if (errorView.getVisibility() == View.VISIBLE) {
            errorView.setVisibility(View.GONE);
        }
        if (web.canGoBack()) {
            web.goBack();
        } else {
            super.onBackPressed();
        }
    }

    @Override
    protected void onSaveInstanceState(Bundle out) {
        super.onSaveInstanceState(out);
        web.saveState(out);
    }

    @Override
    protected void onPause() {
        CookieManager.getInstance().flush();
        super.onPause();
    }
}
