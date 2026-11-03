/** Authorization failure on a retry cannot prove that the earlier request failed. */
export function creationResultUncertain(status: number | undefined, retrying: boolean): boolean {
  return status === undefined || status === 0 || status === 408 || status >= 500
    || (retrying && (status === 401 || status === 403));
}
