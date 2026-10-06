"use client";

import { useEffect, useRef, useState } from "react";
import useSWR from "swr";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";

interface DeviceLoginStart {
  status: "pending";
  attempt_id: string;
  verification_uri_complete: string;
  verification_uri?: string;
  user_code?: string;
}

interface DeviceLoginStatus {
  status: "pending" | "approved" | "failed" | "expired";
}

const pollDelayMilliseconds = 1500;
const cancelTimeoutMilliseconds = 5000;

type APIError = Error & { status?: number };

async function fetchJSON<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const response = await fetch(input, init);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(data.error || "Device login request failed") as APIError;
    error.status = response.status;
    throw error;
  }
  return data as T;
}

export function NativeDeviceLogin() {
  const { t } = useTranslation();
  const [deviceLogin, setDeviceLogin] = useState<DeviceLoginStart | null>(null);
  const [deviceError, setDeviceError] = useState("");
  const [restartGeneration, setRestartGeneration] = useState(0);
  const [attempt, setAttempt] = useState<DeviceLoginStart | null>(null);
  const lifecycleTail = useRef(Promise.resolve());
  const cancellationTail = useRef(Promise.resolve());
  const cancelledAttempts = useRef(new Set<string>());
  const finishingAttempt = useRef("");
  const finishedAttempt = useRef("");

  const statusKey = attempt
    ? `/api/auth/oidc/device/status?attempt_id=${encodeURIComponent(attempt.attempt_id)}`
    : null;
  const { data: status, error: statusError, mutate: refreshStatus } = useSWR<DeviceLoginStatus>(statusKey, (input) => fetchJSON<DeviceLoginStatus>(input), {
    refreshInterval: pollDelayMilliseconds,
    revalidateOnFocus: true,
    revalidateOnReconnect: true,
    dedupingInterval: 500,
    shouldRetryOnError: false,
  });

  const queueCancellation = (attemptID: string) => {
    if (cancelledAttempts.current.has(attemptID)) {
      return cancellationTail.current;
    }
    cancelledAttempts.current.add(attemptID);
    cancellationTail.current = cancellationTail.current.catch(() => undefined).then(async () => {
      const controller = new AbortController();
      const timeout = window.setTimeout(() => controller.abort(), cancelTimeoutMilliseconds);
      try {
        await fetchJSON("/api/auth/oidc/device/cancel", {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ attempt_id: attemptID }),
          signal: controller.signal,
        });
      } catch {
        // The server-side transaction TTL bounds an unreachable cancellation.
      } finally {
        window.clearTimeout(timeout);
      }
    });
    return cancellationTail.current;
  };

  useEffect(() => {
    let active = true;
    let startedAttemptID = "";
    const controller = new AbortController();

    const start = async () => {
      try {
        await cancellationTail.current;
        if (!active) {
          return;
        }
        const data = await fetchJSON<DeviceLoginStart>("/api/auth/oidc/device/start", {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: "{}",
          signal: controller.signal,
        });
        if (!data.attempt_id || !data.verification_uri_complete) {
          throw new Error("Device login is unavailable");
        }
        startedAttemptID = data.attempt_id;
        if (!active) {
          return;
        }
        setDeviceError("");
        setDeviceLogin(data);
        setAttempt(data);
      } catch (error) {
        if (active && !(error instanceof DOMException && error.name === "AbortError")) {
          setDeviceError(error instanceof Error ? error.message : "Device login is unavailable");
        }
      } finally {
        if (!active && startedAttemptID && finishedAttempt.current !== startedAttemptID) {
          await queueCancellation(startedAttemptID);
        }
      }
    };

    lifecycleTail.current = lifecycleTail.current.catch(() => undefined).then(start);

    return () => {
      active = false;
      controller.abort();
      if (startedAttemptID && finishedAttempt.current !== startedAttemptID) {
        void queueCancellation(startedAttemptID);
      }
    };
  }, [restartGeneration]);

  useEffect(() => {
    if (!statusError || !attempt || statusError.name === "AbortError") {
      return;
    }
    setDeviceError(statusError instanceof Error ? statusError.message : "Device login could not be checked");
  }, [attempt, statusError]);

  useEffect(() => {
    if (attempt && (status?.status === "expired" || status?.status === "failed")) {
      setDeviceError("Device login expired or was cancelled");
      setAttempt(null);
    }
  }, [attempt, status]);

  useEffect(() => {
    if (!attempt || status?.status !== "approved" || finishingAttempt.current === attempt.attempt_id || finishedAttempt.current === attempt.attempt_id) {
      return;
    }
    const attemptID = attempt.attempt_id;
    finishingAttempt.current = attemptID;
    const controller = new AbortController();

    const finish = async () => {
      try {
        const response = await fetch("/api/auth/oidc/device/finish", {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ attempt_id: attemptID }),
          signal: controller.signal,
        });
        const data = await response.json().catch(() => ({})) as { authenticated?: boolean; error?: string };
        if (response.status === 200 && data.authenticated === true) {
          finishedAttempt.current = attemptID;
          setAttempt(null);
          window.location.reload();
          return;
        }
        if (response.status === 202) {
          finishingAttempt.current = "";
          await refreshStatus();
          return;
        }
        throw new Error(data.error || "Device login could not be completed");
      } catch (error) {
        if (controller.signal.aborted) {
          return;
        }
        setDeviceError(error instanceof Error ? error.message : "Device login could not be completed");
      }
    };

    void finish();
    return () => controller.abort();
  }, [attempt, refreshStatus, status]);

  const retry = () => {
    finishingAttempt.current = "";
    setAttempt(null);
    setDeviceLogin(null);
    setDeviceError("");
    setRestartGeneration((generation) => generation + 1);
  };

  return (
    <div className="mb-6 space-y-3 text-center">
      <p>{t("login.device_instructions")}</p>
      {deviceLogin && (
        <>
          <a
            href={deviceLogin.verification_uri_complete}
            className="inline-flex w-full items-center justify-center rounded-md bg-primary px-4 py-2 text-primary-foreground hover:bg-primary/90"
          >
            {t("login.device_open")}
          </a>
          {deviceLogin.user_code && <p className="text-sm text-muted-foreground"><code>{deviceLogin.user_code}</code></p>}
        </>
      )}
      {!deviceLogin && !deviceError && <p>{t("login.device_preparing")}</p>}
      {deviceError && (
        <>
          <p className="text-sm text-destructive">{deviceError}</p>
          <Button type="button" variant="outline" onClick={retry}>
            {t("login.device_retry")}
          </Button>
        </>
      )}
    </div>
  );
}
