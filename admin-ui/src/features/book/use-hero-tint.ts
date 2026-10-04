import { useEffect, useState } from 'react';
import { useCover } from '@/api/hooks';
import { coverModel } from '@/lib/cover-model';
import { tintFromPalette, tintFromPixels, type HeroTint } from './tint-model';

const SAMPLE = 24;

/** Draws the art into a tiny canvas and reads its pixels (a data: URL, so never tainted). */
function sample(img: HTMLImageElement): HeroTint | undefined {
  try {
    const canvas = document.createElement('canvas');
    canvas.width = SAMPLE;
    canvas.height = SAMPLE;
    const ctx = canvas.getContext('2d', { willReadFrequently: true });
    if (!ctx) return undefined;
    ctx.drawImage(img, 0, 0, SAMPLE, SAMPLE);
    return tintFromPixels(ctx.getImageData(0, 0, SAMPLE, SAMPLE).data);
  } catch {
    return undefined;
  }
}

/**
 * The hero's tint: sampled from the real art once it has loaded, the
 * generated cover's palette when there is none, and undefined (the neutral
 * fallback in globals.css) while loading or where canvas isn't available.
 */
export function useHeroTint(
  libraryId: number,
  path: string,
  title: string,
  author: string,
): HeroTint | undefined {
  // The backdrop's thumbnail (it is blurred anyway): shared, and plenty to sample.
  const cover = useCover(libraryId, path, 160);
  const art = cover.data;
  const [sampled, setSampled] = useState<{ url: string; tint?: HeroTint }>();

  useEffect(() => {
    if (!art) return;
    let live = true;
    const img = new Image();
    img.onload = () => {
      if (live) setSampled({ url: art, tint: sample(img) });
    };
    img.src = art;
    return () => {
      live = false;
    };
  }, [art]);

  if (cover.isPending) return undefined;
  if (!art) return tintFromPalette(coverModel(title, author).palette);
  return sampled?.url === art ? sampled.tint : undefined;
}
