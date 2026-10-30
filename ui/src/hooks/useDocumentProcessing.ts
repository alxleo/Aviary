import { useEffect, useState } from "react";
import i18n from "@/lib/i18n";

const COMPRESSIBLE_EXTS = [".pdf", ".png", ".jpg", ".jpeg"];

export function useDocumentProcessing(files: ReadonlyArray<{ name: string }>, url: string) {
  const [sniffed, setSniffed] = useState<{ url: string; mime: string | null } | null>(null);

  useEffect(() => {
    if (files.length || !url.trim()) return;
    const controller = new AbortController();
    const sniff = async () => {
      let mime: string | null = null;
      try {
        const response = await fetch(`/api/sniff?url=${encodeURIComponent(url.trim())}`, {
          signal: controller.signal,
          headers: { "Accept-Language": i18n.language },
        });
        if (response.ok) mime = (await response.json()).mime || null;
      } catch {
        // Failed or cancelled inspection leaves scan presets unavailable.
      }
      if (!controller.signal.aborted) setSniffed({ url, mime });
    };
    void sniff();
    return () => controller.abort();
  }, [url, files.length]);

  if (files.length) {
    return {
      compressible: files.some(file => COMPRESSIBLE_EXTS.some(ext => file.name.toLowerCase().endsWith(ext))),
      pdfOnly: files.every(file => file.name.toLowerCase().endsWith(".pdf")),
    };
  }

  const trimmed = url.trim();
  if (!trimmed) return { compressible: true, pdfOnly: false };
  let path = trimmed;
  try {
    path = new URL(trimmed).pathname;
  } catch {
    // The normal submit path reports malformed URLs.
  }
  path = path.toLowerCase();
  const hasExtension = (path.split("/").pop() || "").includes(".");
  const mime = sniffed?.url === url ? sniffed.mime?.toLowerCase() : null;
  const pdfOnly = path.endsWith(".pdf") || (!hasExtension && !!mime?.startsWith("application/pdf"));
  const compressible = COMPRESSIBLE_EXTS.some(ext => path.endsWith(ext)) ||
    (!hasExtension && (!mime || ["application/pdf", "image/png", "image/jpeg"].some(type => mime.startsWith(type))));
  return { compressible, pdfOnly };
}
