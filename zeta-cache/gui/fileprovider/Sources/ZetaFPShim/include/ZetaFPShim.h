#import <Foundation/Foundation.h>
#import <FileProvider/FileProvider.h>

// Availability shim (fileprovider-2026-10 leaf 02).
//
// The macOS 26 SDK annotates NSFileProviderExtension with
// API_UNAVAILABLE(macos) ("explicitly marked unavailable" - the classic
// decorated extension moved off the macOS surface), which makes a Swift
// subclass uncompilable inside an SPM package that imports FileProvider
// directly. This target REDECLARES the same selector surface on an
// NSObject subclass WITHOUT the annotation, so the leaf-02 sources
// compile. The selectors are byte-identical to the real framework's, so
// an override here lands on the same runtime selector; leaf 03's
// pluginkit registration step covers the runtime class identity (a
// registered extension's principal class is looked up by name).
//
// DUPLICATION RULE: this header shadows the framework's declaration;
// when the deployment SDK changes, diff it against
// NSFileProviderExtension.h + NSFileProviderActions.h and update.

NS_ASSUME_NONNULL_BEGIN

@interface ZetaFPShim : NSObject

- (nullable id<NSFileProviderItem>)itemForIdentifier:(NSFileProviderItemIdentifier)identifier
                                               error:(NSError * _Nullable * _Nullable)error;

- (nullable id<NSFileProviderEnumerator>)enumeratorForContainerItemIdentifier:(NSFileProviderItemIdentifier)containerItemIdentifier
                                                                        error:(NSError * _Nullable * _Nullable)error;

- (nullable NSURL *)URLForItemWithPersistentIdentifier:(NSFileProviderItemIdentifier)identifier;
- (nullable NSFileProviderItemIdentifier)persistentIdentifierForItemAtURL:(NSURL *)url;

- (void)startProvidingItemAtURL:(NSURL *)url
              completionHandler:(void (^ _Nonnull)(NSError * _Nullable error))completionHandler;
- (void)stopProvidingItemAtURL:(NSURL *)url;

// NSFileProviderActions surface (leaf actions Finder drives).
- (void)importDocumentAtURL:(NSURL *)fileURL
     toParentItemIdentifier:(NSFileProviderItemIdentifier)parentItemIdentifier
          completionHandler:(void (^ _Nonnull)(id<NSFileProviderItem> _Nullable importedDocumentItem, NSError * _Nullable error))completionHandler
    NS_SWIFT_NAME(importDocument(atFileURL:toParentItemIdentifier:completionHandler:));
- (void)createDirectoryWithName:(NSString *)directoryName
         inParentItemIdentifier:(NSFileProviderItemIdentifier)parentItemIdentifier
              completionHandler:(void (^ _Nonnull)(id<NSFileProviderItem> _Nullable createdDirectoryItem, NSError * _Nullable error))completionHandler
    NS_SWIFT_NAME(createDirectory(withName:inParentItemIdentifier:completionHandler:));
- (void)renameItemWithIdentifier:(NSFileProviderItemIdentifier)itemIdentifier
                          toName:(NSString *)itemName
               completionHandler:(void (^ _Nonnull)(id<NSFileProviderItem> _Nullable renamedItem, NSError * _Nullable error))completionHandler
    NS_SWIFT_NAME(renameItem(withIdentifier:toName:completionHandler:));
- (void)deleteItemWithIdentifier:(NSFileProviderItemIdentifier)itemIdentifier
               completionHandler:(void (^ _Nonnull)(NSError * _Nullable error))completionHandler
    NS_SWIFT_NAME(deleteItem(withIdentifier:completionHandler:));
- (void)deleteItemWithIdentifier:(NSFileProviderItemIdentifier)itemIdentifier
                      baseVersion:(nullable NSFileProviderItemVersion *)baseVersion
                          options:(NSFileProviderDeleteItemOptions)options
                completionHandler:(void (^ _Nonnull)(NSError * _Nullable error))completionHandler
    NS_SWIFT_NAME(deleteItem(withIdentifier:baseVersion:options:completionHandler:));
- (void)evictItemWithIdentifier:(NSFileProviderItemIdentifier)itemIdentifier
              completionHandler:(void (^ _Nonnull)(NSError * _Nullable error))completionHandler;

// The create-with-content surface (createItemBasedOnTemplate: - the
// template/itemTemplate label pair is what the real framework imports).
- (void)createItemBasedOnTemplate:(id<NSFileProviderItem>)itemTemplate
                           fields:(NSFileProviderItemFields)fields
                         contents:(nullable NSURL *)url
                          options:(NSFileProviderCreateItemOptions)options
                completionHandler:(void (^ _Nonnull)(id<NSFileProviderItem> _Nullable item, NSFileProviderItemFields fields, BOOL addResult, NSError * _Nullable error))completionHandler
    NS_SWIFT_NAME(createItemBased(onTemplate:fields:contents:options:completionHandler:));

@property (nullable, nonatomic, readonly) NSFileProviderDomain *domain;

@end

NS_ASSUME_NONNULL_END
