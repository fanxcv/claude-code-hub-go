"use client";

import { Check, ChevronsUpDown } from "lucide-react";
import { useTranslations } from "next-intl";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { searchUsersForFilter } from "@/lib/api-client/v1/actions/users";
import { useDebounce } from "@/lib/hooks/use-debounce";

interface QuickUserFilterProps {
  userId?: number;
  onUserChange: (userId: number | undefined) => void;
}

/**
 * 折叠态筛选栏上的快捷用户下拉：切换即生效（调用方负责改 URL query）。
 *
 * 远端搜索而非只列首屏：`searchUsersForFilter()` 缺省只回一页（后端 limit=20），
 * 用户一多就选不到第一页之外的人，故输入关键字时按词重查。
 * 面板展开后由调用方不渲染，避免与面板内的用户筛选重复。
 */
export function QuickUserFilter({ userId, onUserChange }: QuickUserFilterProps) {
  const t = useTranslations("dashboard");
  const [open, setOpen] = useState(false);
  const [searchTerm, setSearchTerm] = useState("");
  const debouncedSearchTerm = useDebounce(searchTerm, 300);
  const [users, setUsers] = useState<Array<{ id: number; name: string }>>([]);
  // 首帧即处于「搜索中」：否则空列表会先闪一下「无匹配用户」。
  const [isSearching, setIsSearching] = useState(true);
  const [loadFailed, setLoadFailed] = useState(false);
  // 缺省页不一定包含已选用户（远端只回一页），故记住点选时的名字给触发钮用。
  const [selectedUser, setSelectedUser] = useState<{ id: number; name: string } | null>(null);
  const searchRequestIdRef = useRef(0);
  const isMountedRef = useRef(true);

  useEffect(() => {
    isMountedRef.current = true;
    return () => {
      isMountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    const requestId = ++searchRequestIdRef.current;
    setIsSearching(true);
    setLoadFailed(false);

    const loadUsers = async () => {
      try {
        const result = await searchUsersForFilter(debouncedSearchTerm.trim() || undefined);
        // 旧请求的返回不得覆盖新关键字的结果。
        if (!isMountedRef.current || requestId !== searchRequestIdRef.current) return;
        if (result.ok) {
          setUsers(result.data);
        } else {
          console.error("Failed to search users for quick filter:", result.error);
          setUsers([]);
          setLoadFailed(true);
        }
      } catch (error) {
        if (!isMountedRef.current || requestId !== searchRequestIdRef.current) return;
        console.error("Failed to search users for quick filter:", error);
        setUsers([]);
        setLoadFailed(true);
      } finally {
        if (isMountedRef.current && requestId === searchRequestIdRef.current) {
          setIsSearching(false);
        }
      }
    };

    void loadUsers();
  }, [debouncedSearchTerm]);

  // 收起即清搜索词：下次展开回到缺省一页（与面板内用户下拉同惯式）。
  const handleOpenChange = (nextOpen: boolean) => {
    setOpen(nextOpen);
    if (!nextOpen) setSearchTerm("");
  };

  const selectUser = (nextUserId: number | undefined, nextUserName?: string) => {
    onUserChange(nextUserId);
    setSelectedUser(
      nextUserId === undefined || !nextUserName ? null : { id: nextUserId, name: nextUserName }
    );
    handleOpenChange(false);
  };

  const selectedUserName =
    users.find((user) => user.id === userId)?.name ??
    (selectedUser && selectedUser.id === userId ? selectedUser.name : undefined);

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          role="combobox"
          aria-expanded={open}
          aria-label={t("logs.filters.quickUser")}
          className="max-w-[180px] justify-between text-muted-foreground"
        >
          <span className="truncate">
            {userId === undefined
              ? t("logs.filters.allUsers")
              : (selectedUserName ?? String(userId))}
          </span>
          <ChevronsUpDown className="h-4 w-4 shrink-0 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-[320px] p-0" align="start">
        <Command shouldFilter={false}>
          <CommandInput
            placeholder={t("logs.filters.searchUser")}
            value={searchTerm}
            onValueChange={setSearchTerm}
          />
          <CommandList className="max-h-[250px] overflow-y-auto">
            <CommandGroup>
              <CommandItem
                value={t("logs.filters.allUsers")}
                onSelect={() => selectUser(undefined)}
                className="cursor-pointer"
              >
                <span className="flex-1">{t("logs.filters.allUsers")}</span>
                {userId === undefined && <Check className="h-4 w-4 text-primary" />}
              </CommandItem>
              {users.map((user) => (
                <CommandItem
                  key={user.id}
                  value={user.name}
                  onSelect={() => selectUser(user.id, user.name)}
                  className="cursor-pointer"
                >
                  <span className="flex-1">{user.name}</span>
                  {userId === user.id && <Check className="h-4 w-4 text-primary" />}
                </CommandItem>
              ))}
            </CommandGroup>
            {!isSearching && users.length === 0 && (
              <p className="py-6 text-center text-sm text-muted-foreground">
                {loadFailed ? t("logs.error.loadFailed") : t("logs.filters.noUserFound")}
              </p>
            )}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
