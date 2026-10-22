"use client";

import { Check, ChevronsUpDown } from "lucide-react";
import { useTranslations } from "next-intl";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { getKeys } from "@/lib/api-client/v1/actions/keys";
import { searchUsersForFilter } from "@/lib/api-client/v1/actions/users";
import { useDebounce } from "@/lib/hooks/use-debounce";
import type { Key } from "@/types/key";
import type { UsageLogFilters } from "./types";

interface IdentityFiltersProps {
  isAdmin: boolean;
  filters: UsageLogFilters;
  onFiltersChange: (filters: UsageLogFilters) => void;
  initialKeys: Key[];
  isKeysLoading?: boolean;
  onKeysChange?: (keys: Key[]) => void;
  onUsersChange?: (users: Array<{ id: number; name: string }>) => void;
}

export function IdentityFilters({
  isAdmin,
  filters,
  onFiltersChange,
  initialKeys,
  isKeysLoading = false,
  onKeysChange,
  onUsersChange,
}: IdentityFiltersProps) {
  const t = useTranslations("dashboard");

  const [isUsersLoading, setIsUsersLoading] = useState(false);
  const [userSearchTerm, setUserSearchTerm] = useState("");
  const debouncedUserSearchTerm = useDebounce(userSearchTerm, 300);
  const [availableUsers, setAvailableUsers] = useState<Array<{ id: number; name: string }>>([]);
  const userSearchRequestIdRef = useRef(0);
  const keysRequestIdRef = useRef(0);
  const lastLoadedUserSearchTermRef = useRef<string | undefined>(undefined);
  const isMountedRef = useRef(true);

  const [keys, setKeys] = useState<Key[]>(initialKeys);
  const [userPopoverOpen, setUserPopoverOpen] = useState(false);

  const userMap = useMemo(
    () => new Map(availableUsers.map((user) => [user.id, user.name])),
    [availableUsers]
  );

  useEffect(() => {
    isMountedRef.current = true;
    return () => {
      isMountedRef.current = false;
    };
  }, []);

  const loadUsersForFilter = useCallback(
    async (term?: string) => {
      const requestId = ++userSearchRequestIdRef.current;
      setIsUsersLoading(true);
      lastLoadedUserSearchTermRef.current = term;

      try {
        const result = await searchUsersForFilter(term);
        if (!isMountedRef.current || requestId !== userSearchRequestIdRef.current) return;

        if (result.ok) {
          setAvailableUsers(result.data);
          onUsersChange?.(result.data);
        } else {
          console.error("Failed to load users for filter:", result.error);
          setAvailableUsers([]);
        }
      } catch (error) {
        if (!isMountedRef.current || requestId !== userSearchRequestIdRef.current) return;

        console.error("Failed to load users for filter:", error);
        setAvailableUsers([]);
      } finally {
        if (isMountedRef.current && requestId === userSearchRequestIdRef.current) {
          setIsUsersLoading(false);
        }
      }
    },
    [onUsersChange]
  );

  useEffect(() => {
    if (!isAdmin) return;
    void loadUsersForFilter(undefined);
  }, [isAdmin, loadUsersForFilter]);

  useEffect(() => {
    if (!isAdmin || !userPopoverOpen) return;

    const term = debouncedUserSearchTerm.trim() || undefined;
    if (term === lastLoadedUserSearchTermRef.current) return;

    void loadUsersForFilter(term);
  }, [isAdmin, userPopoverOpen, debouncedUserSearchTerm, loadUsersForFilter]);

  useEffect(() => {
    if (!isAdmin) return;
    if (!userPopoverOpen) {
      setUserSearchTerm("");
    }
  }, [isAdmin, userPopoverOpen]);

  useEffect(() => {
    if (initialKeys.length > 0) {
      setKeys(initialKeys);
    }
  }, [initialKeys]);

  // 密钥跟随所选用户：折叠态快捷下拉改 userId 时本组件不重挂（面板 forceMount），
  // 故重拉与清空都必须挂在这里，而不能只挂在面板内的 handleUserChange 上。
  // biome-ignore lint/correctness/useExhaustiveDependencies: 换语言不必重拉密钥；把 t 放进依赖会让「每次渲染新建 t」的测试替身反复重拉。
  useEffect(() => {
    if (!isAdmin) return;

    const requestId = ++keysRequestIdRef.current;
    const targetUserId = filters.userId;

    if (targetUserId === undefined) {
      setKeys([]);
      onKeysChange?.([]);
      return;
    }

    const loadKeysForUser = async () => {
      try {
        const keysResult = await getKeys(targetUserId);
        // 旧用户的慢返回不得覆盖新用户的结果。
        if (!isMountedRef.current || requestId !== keysRequestIdRef.current) return;
        if (keysResult.ok && keysResult.data) {
          setKeys(keysResult.data);
          onKeysChange?.(keysResult.data);
        }
      } catch (error) {
        if (!isMountedRef.current || requestId !== keysRequestIdRef.current) return;
        console.error("Failed to load keys for user:", error);
        toast.error(t("logs.error.loadKeysFailed"));
      }
    };

    void loadKeysForUser();
  }, [isAdmin, filters.userId, onKeysChange]);

  // 面板内换用户只改筛选（URL 语义与清 keyId 不变）；密钥重拉由上面的 effect 跟着 userId 做。
  const handleUserChange = (userId: string) => {
    const newUserId = userId ? parseInt(userId, 10) : undefined;
    onFiltersChange({ ...filters, userId: newUserId, keyId: undefined });
  };

  const handleKeyChange = (value: string) => {
    onFiltersChange({
      ...filters,
      keyId: value && value !== "__all__" ? parseInt(value, 10) : undefined,
    });
  };

  return (
    <div className="grid gap-4 sm:grid-cols-2">
      {/* User selector (Admin only) */}
      {isAdmin && (
        <div className="space-y-2">
          <Label>{t("logs.filters.user")}</Label>
          <Popover open={userPopoverOpen} onOpenChange={setUserPopoverOpen}>
            <PopoverTrigger asChild>
              <Button
                variant="outline"
                role="combobox"
                aria-expanded={userPopoverOpen}
                type="button"
                className="w-full justify-between"
              >
                {filters.userId ? (
                  (userMap.get(filters.userId) ?? filters.userId.toString())
                ) : (
                  <span className="text-muted-foreground">
                    {isUsersLoading ? t("logs.stats.loading") : t("logs.filters.allUsers")}
                  </span>
                )}
                <ChevronsUpDown className="ml-2 h-4 w-4 shrink-0 opacity-50" />
              </Button>
            </PopoverTrigger>
            <PopoverContent
              className="w-[320px] p-0"
              align="start"
              onWheel={(e) => e.stopPropagation()}
              onTouchMove={(e) => e.stopPropagation()}
            >
              <Command shouldFilter={false}>
                <CommandInput
                  placeholder={t("logs.filters.searchUser")}
                  value={userSearchTerm}
                  onValueChange={(value) => setUserSearchTerm(value)}
                />
                <CommandList className="max-h-[250px] overflow-y-auto">
                  <CommandEmpty>
                    {isUsersLoading ? t("logs.stats.loading") : t("logs.filters.noUserFound")}
                  </CommandEmpty>
                  <CommandGroup>
                    <CommandItem
                      value={t("logs.filters.allUsers")}
                      onSelect={() => {
                        handleUserChange("");
                        setUserPopoverOpen(false);
                      }}
                      className="cursor-pointer"
                    >
                      <span className="flex-1">{t("logs.filters.allUsers")}</span>
                      {!filters.userId && <Check className="h-4 w-4 text-primary" />}
                    </CommandItem>
                    {availableUsers.map((user) => (
                      <CommandItem
                        key={user.id}
                        value={user.name}
                        onSelect={() => {
                          handleUserChange(user.id.toString());
                          setUserPopoverOpen(false);
                        }}
                        className="cursor-pointer"
                      >
                        <span className="flex-1">{user.name}</span>
                        {filters.userId === user.id && <Check className="h-4 w-4 text-primary" />}
                      </CommandItem>
                    ))}
                  </CommandGroup>
                </CommandList>
              </Command>
            </PopoverContent>
          </Popover>
        </div>
      )}

      {/* Key selector */}
      <div className="space-y-2">
        <Label>{t("logs.filters.apiKey")}</Label>
        <Select
          value={filters.keyId?.toString() || "__all__"}
          onValueChange={handleKeyChange}
          disabled={isKeysLoading || (isAdmin && !filters.userId && keys.length === 0)}
        >
          <SelectTrigger>
            <SelectValue
              placeholder={
                isKeysLoading
                  ? t("logs.stats.loading")
                  : isAdmin && !filters.userId && keys.length === 0
                    ? t("logs.filters.selectUserFirst")
                    : t("logs.filters.allKeys")
              }
            />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all__">{t("logs.filters.allKeys")}</SelectItem>
            {keys.map((key) => (
              <SelectItem key={key.id} value={key.id.toString()}>
                {key.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  );
}
