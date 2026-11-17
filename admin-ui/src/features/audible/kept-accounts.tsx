import { useTranslation } from 'react-i18next';
import { accountName } from './audible-model';
import { RemoveAccountButton } from './remove-account-button';
import { useAudibleAccounts } from './use-audible';

/**
 * Settings > Modules' Audible card while the module is off (server#294): the
 * accounts this server keeps (their titles and settings wait for the module to
 * come back on), each with Remove, so one no longer used can go without the
 * API. Nothing while there are none, or they can't be read.
 */
export function KeptAudibleAccounts() {
  const { t } = useTranslation();
  const accounts = useAudibleAccounts({ poll: false });
  const list = accounts.data?.accounts ?? [];
  if (list.length === 0) return null;
  return (
    <section aria-labelledby="audible-kept-title" className="border-t px-5 py-4">
      <h3 id="audible-kept-title" className="font-semibold">
        {t('settings.module.audible.kept')}
      </h3>
      <p className="text-[13px] text-muted-foreground">{t('settings.module.audible.keptBody')}</p>
      <ul className="mt-2 flex flex-col divide-y" aria-labelledby="audible-kept-title">
        {list.map((a) => (
          <li key={a.id} className="flex flex-wrap items-center justify-between gap-2 py-2.5">
            <span className="min-w-0 font-[550] [overflow-wrap:anywhere]">
              {accountName(t, a)}
              {/* An account with no label is already named for its person. */}
              {a.label ? (
                <span className="ml-2 font-normal text-muted-foreground">
                  {a.user.display_name}
                </span>
              ) : null}
            </span>
            <RemoveAccountButton account={a} />
          </li>
        ))}
      </ul>
    </section>
  );
}
