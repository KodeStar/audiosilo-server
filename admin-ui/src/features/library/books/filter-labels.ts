import type { TFunction } from 'i18next';
import type { AdminLibrary } from '@/api/types';
import { formatLabel, type ActiveFilter } from './books-model';

/** A filter value as the sheet's chip names it ("Missing cover", "M4B", "Fiction"). */
export function valueLabel(t: TFunction, f: ActiveFilter, libraries: AdminLibrary[]): string {
  switch (f.key) {
    case 'library':
      return libraries.find((l) => String(l.id) === f.value)?.name ?? f.value;
    case 'format':
      return formatLabel(f.value);
    case 'author':
    case 'series':
    case 'narrator':
    case 'codec':
      return f.value;
    default:
      return t(`books.value.${f.key}.${f.value}`);
  }
}

/** An active filter as its chip under the toolbar reads ("Author: Brandon Sanderson"). */
export function chipLabel(t: TFunction, f: ActiveFilter, libraries: AdminLibrary[]): string {
  const value = valueLabel(t, f, libraries);
  switch (f.key) {
    case 'library':
    case 'author':
    case 'series':
    case 'narrator':
    case 'format':
    case 'codec':
    case 'added':
      return t(`books.chip.${f.key}`, { value });
    default:
      return value;
  }
}
