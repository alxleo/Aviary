"use client";

import { useState, useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useAuth } from "@/components/AuthProvider";
import { useConfig } from "@/components/ConfigProvider";

interface LoginFormProps {
  onLogin: () => void;
}

interface DeviceLoginStart {
  status: "pending";
  verification_uri_complete: string;
  verification_uri?: string;
  user_code?: string;
}

export function LoginForm({ onLogin }: LoginFormProps) {
  const { t } = useTranslation();
  const { multiUserMode } = useAuth();
  const { config } = useConfig();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [registrationEnabled, setRegistrationEnabled] = useState(false);
  
  const smtpConfigured = config?.smtpConfigured || false;
  const oidcEnabled = config?.oidcEnabled || false;
  const oidcSsoOnly = config?.oidcSsoOnly || false;
  const oidcButtonText = config?.oidcButtonText || "";
  const proxyAuthEnabled = config?.proxyAuthEnabled || false;
  const nativeApp = typeof document !== "undefined" && document.documentElement.classList.contains("native-app");
  const deviceLoginEnabled = Boolean(nativeApp && multiUserMode && oidcEnabled && config?.oidcDeviceLoginEnabled);
  const [deviceLogin, setDeviceLogin] = useState<DeviceLoginStart | null>(null);
  const [deviceError, setDeviceError] = useState("");
  const [deviceAttempt, setDeviceAttempt] = useState(0);
  const deviceStartRequested = useRef(false);
  const deviceLoginFinished = useRef(false);
  const devicePollTimer = useRef<number | null>(null);

  const cancelDeviceLogin = () => {
    void fetch("/api/auth/oidc/device/cancel", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  };

  useEffect(() => {
    if (!deviceLoginEnabled || deviceStartRequested.current) {
      return;
    }
    deviceStartRequested.current = true;
    let active = true;

    const startDeviceLogin = async () => {
      try {
        const response = await fetch("/api/auth/oidc/device/start", {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: "{}",
        });
        const data = await response.json();
        if (!response.ok || !data.verification_uri_complete) {
          throw new Error(data.error || "Device login is unavailable");
        }
        if (active) {
          setDeviceError("");
          setDeviceLogin(data as DeviceLoginStart);
        }
      } catch (error) {
        if (active) {
          setDeviceError(error instanceof Error ? error.message : "Device login is unavailable");
        }
      }
    };

    void startDeviceLogin();
    return () => {
      active = false;
      if (devicePollTimer.current !== null) {
        window.clearTimeout(devicePollTimer.current);
        devicePollTimer.current = null;
      }
      if (!deviceLoginFinished.current) {
        cancelDeviceLogin();
      }
    };
  }, [deviceAttempt, deviceLoginEnabled]);

  useEffect(() => {
    if (!deviceLogin) {
      return;
    }
    let active = true;

    const schedulePoll = () => {
      if (active) {
        devicePollTimer.current = window.setTimeout(poll, 1500);
      }
    };

    const poll = async () => {
      try {
        const statusResponse = await fetch("/api/auth/oidc/device/status", {
          credentials: "include",
        });
        const statusData = await statusResponse.json();
        if (statusData.status === "approved") {
          const finishResponse = await fetch("/api/auth/oidc/device/finish", {
            method: "POST",
            credentials: "include",
            headers: { "Content-Type": "application/json" },
            body: "{}",
          });
          if (finishResponse.ok) {
            deviceLoginFinished.current = true;
            window.location.reload();
            return;
          }
          if (finishResponse.status !== 202) {
            const finishData = await finishResponse.json().catch(() => ({}));
            throw new Error(finishData.error || "Device login could not be completed");
          }
        } else if (statusData.status === "expired" || statusData.status === "failed") {
          throw new Error("Device login expired or was cancelled");
        }
        schedulePoll();
      } catch (error) {
        if (active) {
          setDeviceError(error instanceof Error ? error.message : "Device login could not be completed");
        }
      }
    };

    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        if (devicePollTimer.current !== null) {
          window.clearTimeout(devicePollTimer.current);
          devicePollTimer.current = null;
        }
        void poll();
      }
    };

    document.addEventListener("visibilitychange", onVisibilityChange);
    void poll();
    return () => {
      active = false;
      document.removeEventListener("visibilitychange", onVisibilityChange);
      if (devicePollTimer.current !== null) {
        window.clearTimeout(devicePollTimer.current);
        devicePollTimer.current = null;
      }
    };
  }, [deviceLogin]);

  const retryDeviceLogin = () => {
    cancelDeviceLogin();
    deviceLoginFinished.current = false;
    deviceStartRequested.current = false;
    setDeviceLogin(null);
    setDeviceError("");
    setDeviceAttempt((attempt) => attempt + 1);
  };

  useEffect(() => {
    // Focus the username field when component mounts
    const usernameInput = document.getElementById("username");
    if (usernameInput) {
      usernameInput.focus();
    }

    // Check registration settings using public endpoint
    const fetchRegistrationStatus = async () => {
      try {
        const registrationResponse = await fetch("/api/auth/registration-status", {
          credentials: "include",
        });
        if (registrationResponse.ok) {
          const registrationData = await registrationResponse.json();
          setRegistrationEnabled(registrationData.enabled || false);
        }
      } catch (error) {
        console.error("Failed to fetch registration status:", error);
      }
    };

    if (multiUserMode) {
      fetchRegistrationStatus();
    }
  }, [multiUserMode]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError("");

    try {
      const response = await fetch("/api/auth/login", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ username, password }),
        credentials: "include",
      });

      if (response.ok) {
        onLogin();
      } else {
        const data = await response.json();
        setError(data.error ? t(data.error) : t("login.fail"));
      }
    } catch {
      setError(t("login.network_error"));
    } finally {
      setLoading(false);
    }
  };

  // Don't show the proxy auth message anymore since we support fallback
  // The form should always be available when proxy auth fails/isn't present

  const isSsoOnly = multiUserMode && oidcEnabled && oidcSsoOnly;

  // Don't render anything until we have config data to prevent flash
  if (multiUserMode && !config) {
    return null;
  }

  return (
    <div className={`bg-background ${isSsoOnly ? 'h-screen flex items-center justify-center px-4 -mt-32 md:-mt-20' : 'pt-0 pb-8 px-8'}`}>
      <div className={isSsoOnly ? 'w-full max-w-md' : ''}>
        <Card className={`${isSsoOnly ? 'w-[95%] mx-auto' : 'max-w-md mx-auto'} bg-card`}>
          {!isSsoOnly && (
            <CardHeader>
              <CardTitle className="text-xl">{t("login.title")}</CardTitle>
            </CardHeader>
          )}
          <CardContent className={isSsoOnly ? 'py-12 px-8' : ''}>
          {/* OIDC Login Button (multi-user mode only) */}
          {deviceLoginEnabled ? (
            <div className="mb-6 space-y-3 text-center">
              <p>{t("login.device_instructions")}</p>
              {deviceLogin && (
                <a
                  href={deviceLogin.verification_uri_complete}
                  className="inline-flex w-full items-center justify-center rounded-md bg-primary px-4 py-2 text-primary-foreground hover:bg-primary/90"
                >
                  {t("login.device_open")}
                </a>
              )}
              {!deviceLogin && !deviceError && <p>{t("login.device_preparing")}</p>}
              {deviceError && (
                <>
                  <p className="text-sm text-destructive">{deviceError}</p>
                  <Button type="button" variant="outline" onClick={retryDeviceLogin}>
                    {t("login.device_retry")}
                  </Button>
                </>
              )}
            </div>
          ) : multiUserMode && oidcEnabled && (
            <div className={isSsoOnly ? 'flex flex-col items-center' : 'mb-6'}>
              <Button 
                type="button" 
                onClick={() => window.location.href = '/api/auth/oidc/login'}
                className="w-full"
                variant={isSsoOnly ? "default" : "outline"}
                disabled={loading}
              >
                {oidcButtonText || t("login.sso_button")}
              </Button>
              {/* Only show divider if not in SSO-only mode */}
              {!oidcSsoOnly && (
                <div className="relative my-4">
                  <div className="absolute inset-0 flex items-center">
                    <span className="w-full border-t" />
                  </div>
                  <div className="relative flex justify-center text-xs uppercase">
                    <span className="bg-card px-2 text-muted-foreground">
                      {t("login.or_continue_with")}
                    </span>
                  </div>
                </div>
              )}
            </div>
          )}
          
          {/* Username/Password form - hidden in SSO-only mode */}
          {!(multiUserMode && oidcEnabled && oidcSsoOnly) && (
            <form onSubmit={handleSubmit} className="space-y-4">
            <div>
              <Label htmlFor="username" className="mb-2 block">
                {t("login.username")}
              </Label>
              <Input
                id="username"
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
                disabled={loading}
              />
            </div>
            <div>
              <Label htmlFor="password" className="mb-2 block">
                {t("login.password")}
              </Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                disabled={loading}
              />
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <div className="flex justify-between items-end">
              <div className="flex flex-col space-y-1">
                {multiUserMode && smtpConfigured && (
                  <Button 
                    type="button" 
                    variant="link" 
                    size="sm"
                    onClick={() => window.location.href = '/reset-password'}
                    disabled={loading}
                    className="text-sm text-muted-foreground hover:text-foreground p-0 h-auto justify-start"
                  >
                    {t("login.forgot_password")}
                  </Button>
                )}
                {multiUserMode && registrationEnabled && (
                  <Button 
                    type="button" 
                    variant="link" 
                    size="sm"
                    onClick={() => window.location.href = '/register'}
                    disabled={loading}
                    className="text-sm text-muted-foreground hover:text-foreground p-0 h-auto justify-start"
                  >
                    {t("login.register")}
                  </Button>
                )}
              </div>
              <Button type="submit" disabled={loading}>
                {loading ? t("login.signing_in") : t("login.button")}
              </Button>
            </div>
          </form>
          )}
        </CardContent>
      </Card>
      </div>
    </div>
  );
}
